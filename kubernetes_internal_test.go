package gubernator

import (
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	discoveryv1 "k8s.io/api/discovery/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func TestExtractPeersFromEndpointSlices(t *testing.T) {
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
			log := logrus.NewEntry(logrus.StandardLogger())

			peers := ExtractPeersFromEndpointSlices(test.slices, test.selfIP, podPort, log)

			require.Len(t, peers, test.wantPeers)

			actualAddrs := make(map[string]bool)
			foundOwner := false
			for _, peer := range peers {
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
