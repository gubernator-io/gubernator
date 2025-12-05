package gubernator

import (
	"reflect"
	"testing"
	"time"
	"unsafe"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	discoveryv1 "k8s.io/api/discovery/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

func TestWatchMechanismFromString(t *testing.T) {
	for _, test := range []struct {
		input    string
		wantMech WatchMechanism
		wantErr  string
	}{
		{
			input:    "",
			wantMech: WatchEndpointSlices,
		},
		{
			input:    "endpointslices",
			wantMech: WatchEndpointSlices,
		},
		{
			input:    "endpoints",
			wantMech: WatchEndpointSlices,
		},
		{
			input:    "pods",
			wantMech: WatchPods,
		},
		{
			input:   "unknown",
			wantErr: "unknown watch mechanism specified: unknown",
		},
	} {
		t.Run(test.input, func(t *testing.T) {
			mech, err := WatchMechanismFromString(test.input)
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.wantMech, mech)
		})
	}
}

func TestK8sPoolUpdatePeersFromEndpointSlices(t *testing.T) {
	const (
		testNamespace = "default"
		serviceName   = "gubernator"
		podPort       = "1051"
		selfIP        = "10.0.0.1"
	)

	boolPtr := func(b bool) *bool {
		return &b
	}

	for _, test := range []struct {
		name      string
		slices    []*discoveryv1.EndpointSlice
		selfIP    string
		wantPeers int
		wantAddrs []string
		wantOwner bool
	}{
		{
			name: "SingleSliceWithReadyEndpoints",
			slices: []*discoveryv1.EndpointSlice{
				{
					ObjectMeta: meta_v1.ObjectMeta{
						Name:      "gubernator-abc",
						Namespace: testNamespace,
						Labels: map[string]string{
							discoveryv1.LabelServiceName: serviceName,
						},
					},
					AddressType: discoveryv1.AddressTypeIPv4,
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses: []string{"10.0.0.1"},
							Conditions: discoveryv1.EndpointConditions{
								Ready: boolPtr(true),
							},
						},
						{
							Addresses: []string{"10.0.0.2"},
							Conditions: discoveryv1.EndpointConditions{
								Ready: boolPtr(true),
							},
						},
					},
				},
			},
			selfIP:    selfIP,
			wantPeers: 2,
			wantAddrs: []string{"10.0.0.1:1051", "10.0.0.2:1051"},
			wantOwner: true,
		},
		{
			name: "SingleSliceWithNotReadyEndpoints",
			slices: []*discoveryv1.EndpointSlice{
				{
					ObjectMeta: meta_v1.ObjectMeta{
						Name:      "gubernator-abc",
						Namespace: testNamespace,
						Labels: map[string]string{
							discoveryv1.LabelServiceName: serviceName,
						},
					},
					AddressType: discoveryv1.AddressTypeIPv4,
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses: []string{"10.0.0.1"},
							Conditions: discoveryv1.EndpointConditions{
								Ready: boolPtr(true),
							},
						},
						{
							Addresses: []string{"10.0.0.2"},
							Conditions: discoveryv1.EndpointConditions{
								Ready: boolPtr(false),
							},
						},
					},
				},
			},
			selfIP:    selfIP,
			wantPeers: 1,
			wantAddrs: []string{"10.0.0.1:1051"},
			wantOwner: true,
		},
		{
			name: "SelfIPInNotReadyState",
			slices: []*discoveryv1.EndpointSlice{
				{
					ObjectMeta: meta_v1.ObjectMeta{
						Name:      "gubernator-abc",
						Namespace: testNamespace,
						Labels: map[string]string{
							discoveryv1.LabelServiceName: serviceName,
						},
					},
					AddressType: discoveryv1.AddressTypeIPv4,
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses: []string{"10.0.0.1"},
							Conditions: discoveryv1.EndpointConditions{
								Ready: boolPtr(false),
							},
						},
						{
							Addresses: []string{"10.0.0.2"},
							Conditions: discoveryv1.EndpointConditions{
								Ready: boolPtr(true),
							},
						},
					},
				},
			},
			selfIP:    selfIP,
			wantPeers: 2,
			wantAddrs: []string{"10.0.0.1:1051", "10.0.0.2:1051"},
			wantOwner: true,
		},
		{
			name: "MultipleSlicesWithOverlappingIPs",
			slices: []*discoveryv1.EndpointSlice{
				{
					ObjectMeta: meta_v1.ObjectMeta{
						Name:      "gubernator-abc",
						Namespace: testNamespace,
						Labels: map[string]string{
							discoveryv1.LabelServiceName: serviceName,
						},
					},
					AddressType: discoveryv1.AddressTypeIPv4,
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses: []string{"10.0.0.1"},
							Conditions: discoveryv1.EndpointConditions{
								Ready: boolPtr(true),
							},
						},
					},
				},
				{
					ObjectMeta: meta_v1.ObjectMeta{
						Name:      "gubernator-def",
						Namespace: testNamespace,
						Labels: map[string]string{
							discoveryv1.LabelServiceName: serviceName,
						},
					},
					AddressType: discoveryv1.AddressTypeIPv4,
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses: []string{"10.0.0.1"},
							Conditions: discoveryv1.EndpointConditions{
								Ready: boolPtr(true),
							},
						},
						{
							Addresses: []string{"10.0.0.2"},
							Conditions: discoveryv1.EndpointConditions{
								Ready: boolPtr(true),
							},
						},
					},
				},
			},
			selfIP:    selfIP,
			wantPeers: 2,
			wantAddrs: []string{"10.0.0.1:1051", "10.0.0.2:1051"},
			wantOwner: true,
		},
		{
			name: "IPv6SlicesAreSkipped",
			slices: []*discoveryv1.EndpointSlice{
				{
					ObjectMeta: meta_v1.ObjectMeta{
						Name:      "gubernator-abc",
						Namespace: testNamespace,
						Labels: map[string]string{
							discoveryv1.LabelServiceName: serviceName,
						},
					},
					AddressType: discoveryv1.AddressTypeIPv4,
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses: []string{"10.0.0.1"},
							Conditions: discoveryv1.EndpointConditions{
								Ready: boolPtr(true),
							},
						},
					},
				},
				{
					ObjectMeta: meta_v1.ObjectMeta{
						Name:      "gubernator-ipv6",
						Namespace: testNamespace,
						Labels: map[string]string{
							discoveryv1.LabelServiceName: serviceName,
						},
					},
					AddressType: discoveryv1.AddressTypeIPv6,
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses: []string{"2001:db8::1"},
							Conditions: discoveryv1.EndpointConditions{
								Ready: boolPtr(true),
							},
						},
					},
				},
			},
			selfIP:    selfIP,
			wantPeers: 1,
			wantAddrs: []string{"10.0.0.1:1051"},
			wantOwner: true,
		},
		{
			name: "NilConditionsMeansReady",
			slices: []*discoveryv1.EndpointSlice{
				{
					ObjectMeta: meta_v1.ObjectMeta{
						Name:      "gubernator-abc",
						Namespace: testNamespace,
						Labels: map[string]string{
							discoveryv1.LabelServiceName: serviceName,
						},
					},
					AddressType: discoveryv1.AddressTypeIPv4,
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses:  []string{"10.0.0.1"},
							Conditions: discoveryv1.EndpointConditions{},
						},
						{
							Addresses: []string{"10.0.0.2"},
						},
					},
				},
			},
			selfIP:    selfIP,
			wantPeers: 2,
			wantAddrs: []string{"10.0.0.1:1051", "10.0.0.2:1051"},
			wantOwner: true,
		},
		{
			name: "EmptyAddressesSkipped",
			slices: []*discoveryv1.EndpointSlice{
				{
					ObjectMeta: meta_v1.ObjectMeta{
						Name:      "gubernator-abc",
						Namespace: testNamespace,
						Labels: map[string]string{
							discoveryv1.LabelServiceName: serviceName,
						},
					},
					AddressType: discoveryv1.AddressTypeIPv4,
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses: []string{},
							Conditions: discoveryv1.EndpointConditions{
								Ready: boolPtr(true),
							},
						},
						{
							Addresses: []string{"10.0.0.1"},
							Conditions: discoveryv1.EndpointConditions{
								Ready: boolPtr(true),
							},
						},
					},
				},
			},
			selfIP:    selfIP,
			wantPeers: 1,
			wantAddrs: []string{"10.0.0.1:1051"},
			wantOwner: true,
		},
		{
			name: "NoSelfIP",
			slices: []*discoveryv1.EndpointSlice{
				{
					ObjectMeta: meta_v1.ObjectMeta{
						Name:      "gubernator-abc",
						Namespace: testNamespace,
						Labels: map[string]string{
							discoveryv1.LabelServiceName: serviceName,
						},
					},
					AddressType: discoveryv1.AddressTypeIPv4,
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses: []string{"10.0.0.2"},
							Conditions: discoveryv1.EndpointConditions{
								Ready: boolPtr(true),
							},
						},
						{
							Addresses: []string{"10.0.0.3"},
							Conditions: discoveryv1.EndpointConditions{
								Ready: boolPtr(true),
							},
						},
					},
				},
			},
			selfIP:    selfIP,
			wantPeers: 2,
			wantAddrs: []string{"10.0.0.2:1051", "10.0.0.3:1051"},
			wantOwner: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()

			for _, slice := range test.slices {
				_, err := client.DiscoveryV1().EndpointSlices(testNamespace).Create(nil, slice, meta_v1.CreateOptions{})
				require.NoError(t, err)
			}

			var capturedPeers []PeerInfo
			onUpdate := func(peers []PeerInfo) {
				capturedPeers = peers
			}

			conf := K8sPoolConfig{
				Logger:      logrus.NewEntry(logrus.StandardLogger()),
				Mechanism:   WatchEndpointSlices,
				OnUpdate:    onUpdate,
				Namespace:   testNamespace,
				ServiceName: serviceName,
				PodIP:       test.selfIP,
				PodPort:     podPort,
			}

			store := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, slice := range test.slices {
				require.NoError(t, store.Add(slice))
			}

			informer := &mockInformer{store: store}

			pool := &K8sPool{}

			setPrivateField(pool, "conf", conf)
			setPrivateField(pool, "client", client)
			setPrivateField(pool, "log", conf.Logger)
			setPrivateField(pool, "informer", informer)

			pool.updatePeersFromEndpointSlices()

			require.Len(t, capturedPeers, test.wantPeers)

			actualAddrs := make(map[string]bool)
			foundOwner := false
			for _, peer := range capturedPeers {
				actualAddrs[peer.GRPCAddress] = true
				if peer.IsOwner {
					foundOwner = true
				}
			}

			for _, wantAddr := range test.wantAddrs {
				assert.True(t, actualAddrs[wantAddr], "expected address %s not found", wantAddr)
			}

			assert.Equal(t, test.wantOwner, foundOwner)
		})
	}
}

func setPrivateField(obj interface{}, fieldName string, value interface{}) {
	v := reflect.ValueOf(obj).Elem()
	f := v.FieldByName(fieldName)
	rf := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()

	rv := reflect.ValueOf(value)
	if rv.Type().AssignableTo(f.Type()) {
		rf.Set(rv)
	} else if rv.Type().ConvertibleTo(f.Type()) {
		rf.Set(rv.Convert(f.Type()))
	} else {
		*(*interface{})(unsafe.Pointer(f.UnsafeAddr())) = value
	}
}

type mockInformer struct {
	store cache.Store
}

func (m *mockInformer) AddEventHandler(handler cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}

func (m *mockInformer) AddEventHandlerWithResyncPeriod(handler cache.ResourceEventHandler, resyncPeriod time.Duration) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}

func (m *mockInformer) RemoveEventHandler(handle cache.ResourceEventHandlerRegistration) error {
	return nil
}

func (m *mockInformer) GetStore() cache.Store {
	return m.store
}

func (m *mockInformer) GetController() cache.Controller {
	return nil
}

func (m *mockInformer) Run(stopCh <-chan struct{}) {
}

func (m *mockInformer) HasSynced() bool {
	return true
}

func (m *mockInformer) LastSyncResourceVersion() string {
	return ""
}

func (m *mockInformer) SetWatchErrorHandler(handler cache.WatchErrorHandler) error {
	return nil
}

func (m *mockInformer) SetTransform(f cache.TransformFunc) error {
	return nil
}

func (m *mockInformer) IsStopped() bool {
	return false
}

func (m *mockInformer) AddIndexers(indexers cache.Indexers) error {
	return nil
}

func (m *mockInformer) GetIndexer() cache.Indexer {
	return nil
}
