# EndpointSlice Migration Implementation Plan

## Overview

Migrate Gubernator's Kubernetes peer discovery from the deprecated `v1 Endpoints` API to the `discovery.k8s.io/v1 EndpointSlice` API. This addresses the deprecation warning: "v1 Endpoints is deprecated in k8s v1.33+; use discovery.k8s.io/v1 EndpointSlice".

## Current State Analysis

**Current implementation** (`kubernetes.go:177-189`):
- Uses `client.CoreV1().Endpoints()` for peer discovery
- Watches Endpoints via `cache.SharedIndexInformer`
- Extracts peer IPs from `endpoint.Subsets[].Addresses[]` and `endpoint.Subsets[].NotReadyAddresses[]`
- Uses label selector (e.g., `app=gubernator`) to filter Endpoints objects
- Supports two watch mechanisms: `endpoints` (default) and `pods`

**Critical API difference for this migration**:

| Aspect | Endpoints | EndpointSlice |
|--------|-----------|---------------|
| Cardinality | 1 Endpoints per Service | N EndpointSlices per Service |
| **Filtering** | **Label selector on Endpoints object** | **Must use `kubernetes.io/service-name=<name>` label** |
| Ready status | Separate arrays (`Addresses` vs `NotReadyAddresses`) | Per-endpoint `conditions.ready` boolean |
| API | `client.CoreV1().Endpoints()` | `client.DiscoveryV1().EndpointSlices()` |

**Key insight**: EndpointSlices do NOT inherit labels from the Pod selector (like `app=gubernator`). They are labeled with `kubernetes.io/service-name=<service-name>` by Kubernetes. This means we need a **new configuration option** to specify the Service name.

**Current client-go version** (`go.mod:38`): `k8s.io/client-go v0.29.13` - fully supports `DiscoveryV1()` EndpointSlice API.

### Key Discoveries:
- `WatchMechanism` type at `kubernetes.go:45-50` defines watch options
- `startEndpointWatch()` at `kubernetes.go:177-189` creates the ListWatch for Endpoints
- `updatePeersFromEndpoints()` at `kubernetes.go:224-259` extracts peer info from Endpoints
- Config env var `GUBER_K8S_ENDPOINTS_SELECTOR` at `config.go:444` - this is a label selector, not a service name
- Service in Helm chart is named via `{{ include "gubernator.fullname" . }}` (`contrib/charts/gubernator/templates/service.yaml:4`)
- RBAC permissions in `contrib/charts/gubernator/templates/rbac.yaml:11-18` and `contrib/k8s-deployment.yaml:96-103`

## Desired End State

After this plan is complete:
1. Gubernator watches `EndpointSlice` resources instead of `Endpoints`
2. New environment variable `GUBER_K8S_SERVICE_NAME` specifies which Service's EndpointSlices to watch
3. The `WatchMechanism` type has `endpointslices` replacing `endpoints`
4. Environment variable `GUBER_K8S_SELECTOR` replaces `GUBER_K8S_ENDPOINTS_SELECTOR` (with backward-compatible alias) for the `pods` watch mechanism
5. RBAC permissions grant access to `endpointslices` in the `discovery.k8s.io` API group
6. All existing tests pass and new tests cover EndpointSlice functionality
7. Documentation is updated to reflect the change

**Verification:**
- Deploy to a K8s 1.33+ cluster and confirm no deprecation warnings in logs
- Peer discovery works correctly with EndpointSlice API
- Existing deployments using `GUBER_K8S_ENDPOINTS_SELECTOR` with pods mechanism continue to work

## What We're NOT Doing

- NOT maintaining backward compatibility with K8s < 1.21 (EndpointSlice GA)
- NOT supporting dual Endpoints/EndpointSlice modes
- NOT changing the `pods` watch mechanism
- NOT modifying the DNS, etcd, or member-list peer discovery mechanisms
- NOT supporting IPv6 EndpointSlices (only IPv4 initially)

## Implementation Approach

Replace the Endpoints-based peer discovery with EndpointSlice-based discovery. The key changes are:
1. Add `ServiceName` field to `K8sPoolConfig` (required for `endpointslices` mechanism)
2. Filter EndpointSlices using `kubernetes.io/service-name=<service-name>` label
3. Handle multiple EndpointSlices per Service (iterate and deduplicate)
4. Check `conditions.ready` per endpoint instead of separate address arrays

The `startGenericWatch()` pattern at `kubernetes.go:112-161` can be reused with a new ListWatch for EndpointSlices.

---

## Phase 1: Update Kubernetes Watch Implementation

### Overview
Replace Endpoints API usage with EndpointSlice API in `kubernetes.go`. Update the `WatchMechanism` constants and implement new watch/update functions.

### Changes Required:

#### 1. Update imports and constants
**File**: `kubernetes.go`
**Changes**: Add EndpointSlice imports, update WatchMechanism constants

```go
import (
	// ... existing imports ...
	discoveryv1 "k8s.io/api/discovery/v1"
)

const (
	WatchEndpointSlices WatchMechanism = "endpointslices"
	WatchPods           WatchMechanism = "pods"
)
```

**Function Responsibilities:**
- Remove `WatchEndpoints` constant
- Add `WatchEndpointSlices` constant as the new default
- Import `k8s.io/api/discovery/v1` for EndpointSlice types

#### 2. Update K8sPoolConfig struct
**File**: `kubernetes.go:65-73`
**Changes**: Add ServiceName field

```go
type K8sPoolConfig struct {
	Logger      FieldLogger
	Mechanism   WatchMechanism
	OnUpdate    UpdateFunc
	Namespace   string
	Selector    string // Label selector for pods mechanism (e.g., "app=gubernator")
	ServiceName string // Service name for endpointslices mechanism
	PodIP       string
	PodPort     string
}
```

**Function Responsibilities:**
- Add `ServiceName` field for EndpointSlice filtering
- Update comment on `Selector` to clarify it's for pods mechanism

#### 3. Update WatchMechanismFromString function
**File**: `kubernetes.go:52-63`
**Changes**: Update mechanism parsing to handle new and legacy values

```go
func WatchMechanismFromString(mechanism string) (WatchMechanism, error)
```

**Function Responsibilities:**
- Return `WatchEndpointSlices` for empty string (default)
- Return `WatchEndpointSlices` for "endpointslices"
- Return `WatchEndpointSlices` for "endpoints" (backward compat - the implementation now uses EndpointSlices)
- Return `WatchPods` for "pods"
- Return error for unknown values

#### 4. Update start() function
**File**: `kubernetes.go:101-110`
**Changes**: Replace `WatchEndpoints` case with `WatchEndpointSlices`

```go
func (e *K8sPool) start() error
```

**Function Responsibilities:**
- Switch on `e.conf.Mechanism`
- Call `startEndpointSliceWatch()` for `WatchEndpointSlices`
- Call `startPodWatch()` for `WatchPods`
- Follow existing error handling pattern

#### 5. Implement startEndpointSliceWatch function
**File**: `kubernetes.go`
**Changes**: New function replacing `startEndpointWatch`

```go
func (e *K8sPool) startEndpointSliceWatch() error
```

**Function Responsibilities:**
- Create `cache.ListWatch` for EndpointSlices
- Use `e.client.DiscoveryV1().EndpointSlices(e.conf.Namespace).List()` for ListFunc
- Use `e.client.DiscoveryV1().EndpointSlices(e.conf.Namespace).Watch()` for WatchFunc
- Set `LabelSelector` to `discoveryv1.LabelServiceName + "=" + e.conf.ServiceName`
- Call `startGenericWatch()` with `&discoveryv1.EndpointSlice{}` and `updatePeersFromEndpointSlices`
- Follow pattern from `startEndpointWatch()` at `kubernetes.go:177-189`

**Example implementation pattern:**
```go
func (e *K8sPool) startEndpointSliceWatch() error {
	listWatch := &cache.ListWatch{
		ListFunc: func(options meta_v1.ListOptions) (runtime.Object, error) {
			options.LabelSelector = discoveryv1.LabelServiceName + "=" + e.conf.ServiceName
			return e.client.DiscoveryV1().EndpointSlices(e.conf.Namespace).List(context.Background(), options)
		},
		WatchFunc: func(options meta_v1.ListOptions) (watch.Interface, error) {
			options.LabelSelector = discoveryv1.LabelServiceName + "=" + e.conf.ServiceName
			return e.client.DiscoveryV1().EndpointSlices(e.conf.Namespace).Watch(e.watchCtx, options)
		},
	}
	return e.startGenericWatch(&discoveryv1.EndpointSlice{}, listWatch, e.updatePeersFromEndpointSlices)
}
```

#### 6. Implement updatePeersFromEndpointSlices function
**File**: `kubernetes.go`
**Changes**: New function replacing `updatePeersFromEndpoints`

```go
func (e *K8sPool) updatePeersFromEndpointSlices()
```

**Function Responsibilities:**
- Use a map to track unique peers by IP (deduplication across multiple slices)
- Iterate over all EndpointSlice objects from informer store
- Skip EndpointSlices with `AddressType != discoveryv1.AddressTypeIPv4`
- For each EndpointSlice, iterate over `slice.Endpoints`
- For each endpoint, determine ready state:
  - `isReady := endpoint.Conditions == nil || endpoint.Conditions.Ready == nil || *endpoint.Conditions.Ready`
  - Note: nil conditions means ready (per K8s convention for control plane generated slices)
- Extract IP from `endpoint.Addresses[0]` (use first address)
- Build `PeerInfo` with `GRPCAddress: fmt.Sprintf("%s:%s", ip, e.conf.PodPort)`
- Set `IsOwner: true` if IP matches `e.conf.PodIP`
- Include endpoint if ready OR if it's our own IP (self-discovery for health check)
- Convert map values to slice and call `e.conf.OnUpdate(peers)`

**Example implementation pattern:**
```go
func (e *K8sPool) updatePeersFromEndpointSlices() {
	e.log.Debug("Fetching peer list from endpointslices API")

	// Use map for deduplication across multiple slices
	peerMap := make(map[string]PeerInfo)

	for _, obj := range e.informer.GetStore().List() {
		slice, ok := obj.(*discoveryv1.EndpointSlice)
		if !ok {
			e.log.Errorf("expected type discoveryv1.EndpointSlice got '%s' instead", reflect.TypeOf(obj).String())
			continue
		}

		// Only process IPv4 slices
		if slice.AddressType != discoveryv1.AddressTypeIPv4 {
			continue
		}

		for _, endpoint := range slice.Endpoints {
			if len(endpoint.Addresses) == 0 {
				continue
			}

			ip := endpoint.Addresses[0]

			// Determine ready state (nil means ready for control plane slices)
			isReady := endpoint.Conditions == nil ||
			           endpoint.Conditions.Ready == nil ||
			           *endpoint.Conditions.Ready

			isOwner := ip == e.conf.PodIP

			// Include if ready OR if it's our own IP (for health check)
			if !isReady && !isOwner {
				e.log.Debugf("Skipping peer because it's not ready: %s", ip)
				continue
			}

			peer := PeerInfo{
				GRPCAddress: fmt.Sprintf("%s:%s", ip, e.conf.PodPort),
				IsOwner:     isOwner,
			}

			// Dedup: if already exists, prefer ready over not-ready
			if existing, exists := peerMap[ip]; exists {
				if !existing.IsOwner && isOwner {
					peerMap[ip] = peer // Update to mark as owner
				}
				continue
			}

			peerMap[ip] = peer
			e.log.Debugf("Peer: %+v", peer)
		}
	}

	peers := make([]PeerInfo, 0, len(peerMap))
	for _, peer := range peerMap {
		peers = append(peers, peer)
	}

	e.conf.OnUpdate(peers)
}
```

#### 7. Remove deprecated functions
**File**: `kubernetes.go`
**Changes**: Remove `startEndpointWatch` and `updatePeersFromEndpoints` functions

**Function Responsibilities:**
- Delete `startEndpointWatch()` function (lines 177-189)
- Delete `updatePeersFromEndpoints()` function (lines 224-259)

**Testing Requirements:**
```go
func TestWatchMechanismFromString(t *testing.T)
func TestK8sPool_UpdatePeersFromEndpointSlices(t *testing.T)
```

**Test Objectives:**
- Verify `WatchMechanismFromString` returns correct mechanism for all inputs:
  - Empty string -> `WatchEndpointSlices`
  - "endpointslices" -> `WatchEndpointSlices`
  - "endpoints" -> `WatchEndpointSlices` (backward compat)
  - "pods" -> `WatchPods`
  - Unknown -> error
- Verify EndpointSlice parsing extracts peers correctly
- Verify ready/not-ready filtering works correctly
- Verify self-discovery works even when not ready
- Verify deduplication across multiple EndpointSlices
- Verify IPv4-only filtering

**Testing Strategy:**
- Use `k8s.io/client-go/kubernetes/fake` for fake clientset
- Create test EndpointSlice objects with various scenarios:
  - Single slice with ready endpoints
  - Single slice with not-ready endpoints
  - Multiple slices with overlapping IPs
  - Self IP in not-ready state
  - IPv6 slices (should be skipped)

**Context for implementation:**
- Existing watch pattern at `kubernetes.go:112-161` (`startGenericWatch`)
- Existing peer extraction pattern at `kubernetes.go:224-259` (`updatePeersFromEndpoints`)
- `discoveryv1.LabelServiceName` constant = `"kubernetes.io/service-name"`
- `discoveryv1.AddressTypeIPv4` for filtering

### Validation
- [ ] Run: `go build ./...`
- [ ] Run: `go test ./... -run TestWatchMechanism`
- [ ] Verify: Code compiles without errors

---

## Phase 2: Update Configuration and Environment Variables

### Overview
Add `GUBER_K8S_SERVICE_NAME` as a new required env var for EndpointSlice discovery. Add `GUBER_K8S_SELECTOR` as a rename of `GUBER_K8S_ENDPOINTS_SELECTOR` with backward compatibility.

### Changes Required:

#### 1. Update config loading for service name
**File**: `config.go:440-450`
**Changes**: Add service name loading and validation

```go
// In SetupDaemonConfig function, after line 444
```

**Function Responsibilities:**
- Load `GUBER_K8S_SERVICE_NAME` env var into `conf.K8PoolConf.ServiceName`
- If using k8s discovery with endpointslices mechanism and `ServiceName` is empty, return error
- Keep `Selector` for pods mechanism

#### 2. Update selector loading with env var aliasing
**File**: `config.go:444`
**Changes**: Load from GUBER_K8S_SELECTOR with GUBER_K8S_ENDPOINTS_SELECTOR as fallback

**Function Responsibilities:**
- Check `GUBER_K8S_SELECTOR` first
- Fall back to `GUBER_K8S_ENDPOINTS_SELECTOR` if `GUBER_K8S_SELECTOR` is empty
- Log deprecation warning if only `GUBER_K8S_ENDPOINTS_SELECTOR` is used

**Example:**
```go
// Load selector with backward compatibility
selector := os.Getenv("GUBER_K8S_SELECTOR")
if selector == "" {
	selector = os.Getenv("GUBER_K8S_ENDPOINTS_SELECTOR")
	if selector != "" {
		log.Warn("GUBER_K8S_ENDPOINTS_SELECTOR is deprecated, use GUBER_K8S_SELECTOR instead")
	}
}
conf.K8PoolConf.Selector = selector

// Load service name (required for endpointslices)
conf.K8PoolConf.ServiceName = os.Getenv("GUBER_K8S_SERVICE_NAME")
```

#### 3. Update validation
**File**: `config.go:482-488`
**Changes**: Update validation to check for required config based on mechanism

**Function Responsibilities:**
- If mechanism is `endpointslices` (or empty/endpoints), require `ServiceName`
- If mechanism is `pods`, require `Selector`
- Update error messages to reference new env var names

**Example:**
```go
if anyHasPrefix("GUBER_K8S_", os.Environ()) {
	log.Debug("K8s peer pool config found")

	mechanism := conf.K8PoolConf.Mechanism
	if mechanism == WatchEndpointSlices || mechanism == "" {
		if conf.K8PoolConf.ServiceName == "" {
			return conf, errors.New("when using k8s for peer discovery with endpointslices, " +
				"you MUST provide `GUBER_K8S_SERVICE_NAME` to identify the Service")
		}
	} else if mechanism == WatchPods {
		if conf.K8PoolConf.Selector == "" {
			return conf, errors.New("when using k8s for peer discovery with pods, " +
				"you MUST provide `GUBER_K8S_SELECTOR` to select the gubernator pods")
		}
	}
}
```

#### 4. Update watch mechanism error message
**File**: `config.go:448-450`
**Changes**: Update error message to reflect new default

**Function Responsibilities:**
- Update error message to say "endpointslices" or "pods" instead of "endpoints" or "pods"

**Testing Requirements:**
```go
// Existing tests that may require updates:
func TestSetupDaemonConfig(t *testing.T)  // May need: updated env var assertions and new test cases
```

**Test Objectives:**
- Verify `GUBER_K8S_SERVICE_NAME` is correctly loaded
- Verify `GUBER_K8S_SELECTOR` takes precedence over `GUBER_K8S_ENDPOINTS_SELECTOR`
- Verify backward compatibility with `GUBER_K8S_ENDPOINTS_SELECTOR` alone (with deprecation warning)
- Verify validation fails if `ServiceName` missing for endpointslices mechanism
- Verify validation fails if `Selector` missing for pods mechanism

**Context for implementation:**
- Existing env var loading pattern at `config.go:440-450`
- `setter.SetDefault` pattern used throughout config loading
- Log deprecation warnings using `log.Warn()` pattern

### Validation
- [ ] Run: `go test ./... -run TestSetup`
- [ ] Verify: Both env vars work correctly
- [ ] Verify: Deprecation warning logged for old env var
- [ ] Verify: Validation errors have clear messages

---

## Phase 3: Update RBAC and Deployment Manifests

### Overview
Update Kubernetes RBAC rules and deployment manifests to grant access to EndpointSlice resources in the `discovery.k8s.io` API group. Add the new `GUBER_K8S_SERVICE_NAME` environment variable.

### Changes Required:

#### 1. Update Helm chart RBAC
**File**: `contrib/charts/gubernator/templates/rbac.yaml`
**Changes**: Change API group and resource for EndpointSlice access

```yaml
rules:
  - apiGroups:
      - "discovery.k8s.io"
    resources:
      - endpointslices
    verbs:
      - list
      - watch
```

**Function Responsibilities:**
- Change `apiGroups` from `""` to `"discovery.k8s.io"`
- Change `resources` from `endpoints` to `endpointslices`
- Keep verbs as `list` and `watch`

#### 2. Update Helm chart env template
**File**: `contrib/charts/gubernator/templates/_helpers.tpl:86-119`
**Changes**: Add GUBER_K8S_SERVICE_NAME, update watch mechanism default

```yaml
- name: GUBER_K8S_SERVICE_NAME
  value: "{{ include "gubernator.fullname" . }}"
- name: GUBER_K8S_SELECTOR
  value: "app.kubernetes.io/name={{ include "gubernator.name" . }},app.kubernetes.io/instance={{ .Release.Name }}"
- name: GUBER_K8S_WATCH_MECHANISM
{{- if .Values.gubernator.watchPods }}
  value: "pods"
{{- else }}
  value: "endpointslices"
{{- end }}
```

**Function Responsibilities:**
- Add `GUBER_K8S_SERVICE_NAME` using the fullname template (matches Service name)
- Rename `GUBER_K8S_ENDPOINTS_SELECTOR` to `GUBER_K8S_SELECTOR`
- Update selector value to use `gubernator.selectorLabels` (matches Service selector)
- Change default watch mechanism from "endpoints" to "endpointslices"

#### 3. Update standalone deployment RBAC
**File**: `contrib/k8s-deployment.yaml:92-115`
**Changes**: Update Role rules for EndpointSlice access and rename Role

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: gubernator
rules:
- apiGroups:
  - "discovery.k8s.io"
  resources:
  - endpointslices
  verbs:
  - list
  - watch
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: gubernator
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: gubernator
subjects:
- kind: ServiceAccount
  name: gubernator
```

**Function Responsibilities:**
- Rename Role from `get-endpoints` to `gubernator`
- Change `apiGroups` from `""` to `"discovery.k8s.io"`
- Change `resources` from `endpoints` to `endpointslices`
- Keep verbs as `list` and `watch`
- Update RoleBinding to reference new Role name

#### 4. Update standalone deployment env vars
**File**: `contrib/k8s-deployment.yaml:28-59`
**Changes**: Add GUBER_K8S_SERVICE_NAME, rename selector env var

```yaml
- name: GUBER_K8S_SERVICE_NAME
  value: "gubernator"
- name: GUBER_K8S_SELECTOR
  value: "app=gubernator"
# Remove or comment out GUBER_K8S_ENDPOINTS_SELECTOR
# Remove or comment out GUBER_K8S_WATCH_MECHANISM (defaults to endpointslices)
```

**Function Responsibilities:**
- Add `GUBER_K8S_SERVICE_NAME` with value matching the Service name (`gubernator`)
- Rename `GUBER_K8S_ENDPOINTS_SELECTOR` to `GUBER_K8S_SELECTOR`
- Remove or comment out `GUBER_K8S_WATCH_MECHANISM` (default is now endpointslices)
- Update comments to explain the new configuration

**Testing Requirements:**
- Manual validation required (Kubernetes manifests)

**Test Objectives:**
- Deploy to test cluster and verify peer discovery works
- Verify no RBAC permission errors in logs
- Verify correct EndpointSlices are watched (filter by service name)

**Context for implementation:**
- Current RBAC at `contrib/charts/gubernator/templates/rbac.yaml:11-18`
- Current standalone RBAC at `contrib/k8s-deployment.yaml:92-115`
- Service name in Helm: `{{ include "gubernator.fullname" . }}`
- Service name in standalone: `gubernator`

### Validation
- [ ] Run: `helm template ./contrib/charts/gubernator` (verify no errors)
- [ ] Run: `kubectl apply --dry-run=client -f contrib/k8s-deployment.yaml`
- [ ] Verify: RBAC rules reference correct API group and resource
- [ ] Verify: GUBER_K8S_SERVICE_NAME is set correctly

---

## Phase 4: Update Documentation

### Overview
Update README and any other documentation to reflect the migration from Endpoints to EndpointSlice.

### Changes Required:

#### 1. Update README Kubernetes section
**File**: `README.md`
**Changes**: Update references to Endpoints API and environment variables

**Function Responsibilities:**
- Update any mention of "Endpoints API" to "EndpointSlice API"
- Document `GUBER_K8S_SERVICE_NAME` as the required env var for k8s discovery
- Document `GUBER_K8S_SELECTOR` as the primary selector env var
- Note that `GUBER_K8S_ENDPOINTS_SELECTOR` is deprecated but supported
- Update the environment variables table with new/changed vars
- Update any example configurations

#### 2. Update architecture documentation
**File**: `docs/architecture.md`
**Changes**: Update peer discovery documentation

**Function Responsibilities:**
- Update references to Endpoints to mention EndpointSlice
- Document the label-based association (`kubernetes.io/service-name`)
- Explain that a Service must exist for EndpointSlice discovery to work

#### 3. Update example config if exists
**File**: `example.conf` (if exists)
**Changes**: Update K8s config examples

**Function Responsibilities:**
- Update env var names in examples
- Add `GUBER_K8S_SERVICE_NAME` to examples
- Add comments about deprecated env vars

**Testing Requirements:**
- Manual review of documentation accuracy

**Context for implementation:**
- Current README references at lines mentioning Kubernetes
- Architecture docs at `docs/architecture.md`

### Validation
- [ ] Review: All documentation accurately reflects new behavior
- [ ] Verify: New env vars are documented
- [ ] Verify: Migration path from old env vars is clear

---

## Migration Guide

### For existing deployments

1. **Add `GUBER_K8S_SERVICE_NAME`**: Set this to the name of your Kubernetes Service that selects Gubernator pods. This is the most important change.

2. **Optional: Rename selector env var**: Replace `GUBER_K8S_ENDPOINTS_SELECTOR` with `GUBER_K8S_SELECTOR`. The old name still works but logs a deprecation warning.

3. **Update RBAC**: Your Role must grant access to `endpointslices` in the `discovery.k8s.io` API group instead of `endpoints` in the core API group.

4. **Watch mechanism**: The value "endpoints" is still accepted and will use EndpointSlices. No change needed unless you want to explicitly set "endpointslices".

### Example migration

**Before:**
```yaml
env:
- name: GUBER_K8S_ENDPOINTS_SELECTOR
  value: "app=gubernator"
- name: GUBER_K8S_WATCH_MECHANISM
  value: "endpoints"

# RBAC
rules:
- apiGroups: [""]
  resources: ["endpoints"]
  verbs: ["list", "watch"]
```

**After:**
```yaml
env:
- name: GUBER_K8S_SERVICE_NAME
  value: "gubernator"  # Must match your Service name
- name: GUBER_K8S_SELECTOR
  value: "app=gubernator"  # Only needed if using pods mechanism
# GUBER_K8S_WATCH_MECHANISM defaults to "endpointslices"

# RBAC
rules:
- apiGroups: ["discovery.k8s.io"]
  resources: ["endpointslices"]
  verbs: ["list", "watch"]
```

---

## Summary

| Phase | Description | Key Files |
|-------|-------------|-----------|
| 1 | Update K8s watch implementation | `kubernetes.go` |
| 2 | Update config and env vars | `config.go` |
| 3 | Update RBAC and manifests | `contrib/charts/gubernator/templates/rbac.yaml`, `contrib/charts/gubernator/templates/_helpers.tpl`, `contrib/k8s-deployment.yaml` |
| 4 | Update documentation | `README.md`, `docs/architecture.md` |

**Key new configuration:**
- `GUBER_K8S_SERVICE_NAME` - Required for EndpointSlice discovery, specifies which Service to watch
- `GUBER_K8S_SELECTOR` - Replaces `GUBER_K8S_ENDPOINTS_SELECTOR` (old name still works with deprecation warning)

**Total estimated changes:**
- ~120 lines modified in `kubernetes.go`
- ~30 lines modified in `config.go`
- ~15 lines modified in each manifest file
- Documentation updates as needed
