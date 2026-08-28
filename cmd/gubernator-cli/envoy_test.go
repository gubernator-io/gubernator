/*
Copyright 2018-2022 Mailgun Technologies Inc

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	guber "github.com/gubernator-io/gubernator/v2"
	"github.com/gubernator-io/gubernator/v2/cluster"
	cli "github.com/gubernator-io/gubernator/v2/cmd/gubernator-cli"
	"github.com/gubernator-io/gubernator/v2/envoy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gopkg.in/yaml.v3"
)

func TestMain(m *testing.M) {
	var peers []guber.PeerInfo
	port := 3111
	for i := 0; i < 2; i++ {
		peers = append(peers, guber.PeerInfo{
			HTTPAddress: fmt.Sprintf("localhost:%d", port),
			GRPCAddress: fmt.Sprintf("localhost:%d", port+1),
		})
		port += 2
	}
	err := cluster.StartWith(peers, cluster.WithEnvoy(guber.EnvoyConfig{Enabled: true, RegisterRLS: envoy.Register}))
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	code := m.Run()
	cluster.Stop()
	os.Exit(code)
}

func policyClient(t *testing.T) guber.EnvoyPolicyV1Client {
	t.Helper()
	conn, err := grpc.NewClient(cluster.PeerAt(0).GRPCAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return guber.NewEnvoyPolicyV1Client(conn)
}

func listPolicies(t *testing.T) map[string]*guber.DomainPolicy {
	t.Helper()
	resp, err := policyClient(t).ListPolicies(context.Background(), &guber.ListPoliciesReq{})
	require.NoError(t, err)
	out := make(map[string]*guber.DomainPolicy)
	for _, p := range resp.Policies {
		out[p.Domain] = p
	}
	return out
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func run(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err = cli.Run(context.Background(), append([]string{args[0], args[1], "-e", cluster.PeerAt(0).GRPCAddress}, args[2:]...),
		cli.Options{Stdout: &out, Stderr: &errOut})
	return out.String(), errOut.String(), err
}

// yamlPolicies mirrors the file format `envoy apply` reads and `envoy get` prints.
type yamlPolicies struct {
	Policies []struct {
		Domain         string   `yaml:"domain"`
		Algorithm      string   `yaml:"algorithm"`
		Behaviors      []string `yaml:"behaviors"`
		OnMissingLimit string   `yaml:"on_missing_limit"`
		Version        int64    `yaml:"version"`
		Origin         string   `yaml:"origin"`
	} `yaml:"policies"`
}

func TestApplyMatchesEquivalentRPC(t *testing.T) {
	file := writeFile(t, `
policies:
  - domain: cli-checkout
    algorithm: LEAKY_BUCKET
    behaviors: [GLOBAL, DURATION_IS_GREGORIAN]
    on_missing_limit: error
  - domain: cli-billing
`)
	stdout, _, err := run(t, "envoy", "apply", "-f", file)
	require.NoError(t, err)
	assert.Contains(t, stdout, "cli-checkout")
	assert.Contains(t, stdout, "cli-billing")
	assert.Contains(t, stdout, "version")

	_, err = policyClient(t).ApplyPolicies(context.Background(), &guber.ApplyPoliciesReq{
		Policies: []*guber.DomainPolicy{
			{
				Domain:         "rpc-checkout",
				Algorithm:      guber.Algorithm_LEAKY_BUCKET,
				Behavior:       int32(guber.Behavior_GLOBAL | guber.Behavior_DURATION_IS_GREGORIAN),
				OnMissingLimit: guber.MissingLimitAction_ERROR,
			},
			{Domain: "rpc-billing"},
		},
	})
	require.NoError(t, err)

	got := listPolicies(t)
	for _, pair := range [][2]string{{"cli-checkout", "rpc-checkout"}, {"cli-billing", "rpc-billing"}} {
		fromCLI, fromRPC := got[pair[0]], got[pair[1]]
		require.NotNil(t, fromCLI)
		require.NotNil(t, fromRPC)
		assert.Equal(t, fromRPC.Algorithm, fromCLI.Algorithm)
		assert.Equal(t, fromRPC.Behavior, fromCLI.Behavior)
		assert.Equal(t, fromRPC.OnMissingLimit, fromCLI.OnMissingLimit)
		assert.Equal(t, fromRPC.Origin, fromCLI.Origin)
		assert.NotZero(t, fromCLI.Version)
	}
}

func TestApplyRejectsUnknownKeysBeforeAnyRPC(t *testing.T) {
	for _, test := range []struct {
		name string
		yaml string
		key  string
	}{
		{
			name: "ShadowMode",
			key:  "shadow_mode",
			yaml: "policies:\n  - domain: unknown-key-shadow\n    shadow_mode: true\n",
		},
		{
			name: "Unlimited",
			key:  "unlimited",
			yaml: "policies:\n  - domain: unknown-key-unlimited\n    unlimited: true\n",
		},
		{
			name: "RateLimit",
			key:  "rate_limit",
			yaml: "policies:\n  - domain: unknown-key-ratelimit\n    rate_limit:\n      unit: minute\n      requests_per_unit: 5\n",
		},
		{
			name: "TopLevel",
			key:  "descriptors",
			yaml: "domain: unknown-key-top\ndescriptors: []\npolicies:\n  - domain: unknown-key-top\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := run(t, "envoy", "apply", "-f", writeFile(t, test.yaml))
			require.Error(t, err)
			assert.ErrorContains(t, err, test.key)
			for domain := range listPolicies(t) {
				assert.NotContains(t, domain, "unknown-key")
			}
		})
	}
}

func TestApplyRejectsInvalidValues(t *testing.T) {
	for _, test := range []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "UnknownAlgorithm",
			yaml:    "policies:\n  - domain: bad-value\n    algorithm: SLIDING_WINDOW\n",
			wantErr: "SLIDING_WINDOW",
		},
		{
			name:    "UnknownBehavior",
			yaml:    "policies:\n  - domain: bad-value\n    behaviors: [TURBO]\n",
			wantErr: "TURBO",
		},
		{
			name:    "UnknownMissingLimit",
			yaml:    "policies:\n  - domain: bad-value\n    on_missing_limit: shrug\n",
			wantErr: "shrug",
		},
		{
			name:    "EmptyDomain",
			yaml:    "policies:\n  - algorithm: TOKEN_BUCKET\n",
			wantErr: "domain",
		},
		{
			name:    "NoPolicies",
			yaml:    "policies: []\n",
			wantErr: "no policies",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := run(t, "envoy", "apply", "-f", writeFile(t, test.yaml))
			require.Error(t, err)
			assert.ErrorContains(t, err, test.wantErr)
			assert.NotContains(t, listPolicies(t), "bad-value")
		})
	}
}

func TestGetPrintsEveryPolicyAsYAML(t *testing.T) {
	_, _, err := run(t, "envoy", "apply", "-f", writeFile(t, `
policies:
  - domain: get-one
    algorithm: LEAKY_BUCKET
    behaviors: [GLOBAL]
    on_missing_limit: allow
  - domain: get-two
`))
	require.NoError(t, err)

	stdout, _, err := run(t, "envoy", "get")
	require.NoError(t, err)

	var printed yamlPolicies
	require.NoError(t, yaml.Unmarshal([]byte(stdout), &printed))
	want := listPolicies(t)
	assert.Len(t, printed.Policies, len(want))
	seen := make(map[string]bool)
	for _, p := range printed.Policies {
		seen[p.Domain] = true
		require.Contains(t, want, p.Domain)
		assert.Equal(t, want[p.Domain].Version, p.Version)
		assert.Equal(t, want[p.Domain].Origin, p.Origin)
		switch p.Domain {
		case "get-one":
			assert.Equal(t, "LEAKY_BUCKET", p.Algorithm)
			assert.Equal(t, []string{"GLOBAL"}, p.Behaviors)
			assert.Equal(t, "allow", p.OnMissingLimit)
		case "get-two":
			assert.Equal(t, "TOKEN_BUCKET", p.Algorithm)
			assert.Empty(t, p.Behaviors)
			assert.Equal(t, "deny", p.OnMissingLimit)
		}
	}
	assert.True(t, seen["get-one"])
	assert.True(t, seen["get-two"])
}

func TestDeleteRemovesDomains(t *testing.T) {
	_, _, err := run(t, "envoy", "apply", "-f", writeFile(t, "policies:\n  - domain: del-one\n  - domain: del-two\n"))
	require.NoError(t, err)
	require.Contains(t, listPolicies(t), "del-one")

	_, _, err = run(t, "envoy", "delete", "del-one", "del-two")
	require.NoError(t, err)
	got := listPolicies(t)
	assert.NotContains(t, got, "del-one")
	assert.NotContains(t, got, "del-two")

	_, _, err = run(t, "envoy", "delete")
	require.Error(t, err)
}

func TestUnknownSubcommandFails(t *testing.T) {
	_, _, err := run(t, "envoy", "frobnicate")
	require.Error(t, err)
	_, _, err = run(t, "envoy", "apply")
	require.Error(t, err)
	assert.ErrorContains(t, err, "-f")
}

func TestDeleteAcceptsDomainsStartingWithDash(t *testing.T) {
	_, _, err := run(t, "envoy", "apply", "-f", writeFile(t, "policies:\n  - domain: -internal-svc\n  - domain: -other\n"))
	require.NoError(t, err)
	require.Contains(t, listPolicies(t), "-internal-svc")

	// Everything after "--" is a domain, however many there are
	_, _, err = run(t, "envoy", "delete", "--", "-internal-svc", "-other")
	require.NoError(t, err)
	got := listPolicies(t)
	assert.NotContains(t, got, "-internal-svc")
	assert.NotContains(t, got, "-other")
}
