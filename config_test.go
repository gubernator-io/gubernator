package gubernator

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func TestParsesGrpcAddress(t *testing.T) {
	os.Clearenv()
	s := `
# a comment
GUBER_GRPC_ADDRESS=10.10.10.10:9000`
	daemonConfig, err := SetupDaemonConfig(logrus.StandardLogger(), strings.NewReader(s))
	require.NoError(t, err)
	require.Equal(t, "10.10.10.10:9000", daemonConfig.GRPCListenAddress)
	require.NotEmpty(t, daemonConfig.InstanceID)
}

func TestDefaultListenAddress(t *testing.T) {
	os.Clearenv()
	s := `
# a comment`
	daemonConfig, err := SetupDaemonConfig(logrus.StandardLogger(), strings.NewReader(s))
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%s:1051", LocalHost()), daemonConfig.GRPCListenAddress)
	require.Equal(t, fmt.Sprintf("%s:1050", LocalHost()), daemonConfig.HTTPListenAddress)
	require.NotEmpty(t, daemonConfig.InstanceID)
}

func TestDefaultInstanceId(t *testing.T) {
	os.Clearenv()
	s := ``
	daemonConfig, err := SetupDaemonConfig(logrus.StandardLogger(), strings.NewReader(s))
	require.NoError(t, err)
	require.NotEmpty(t, daemonConfig.InstanceID)

	instanceConfig := Config{}
	err = instanceConfig.SetDefaults()
	require.NoError(t, err)
	require.NotEmpty(t, instanceConfig.InstanceID)
}

func TestConfig_AdvertiseAddrIsSpecified(t *testing.T) {
	tests := []struct {
		name          string
		advertiseAddr string
		want          bool
	}{
		{
			name:          "DNS name",
			advertiseAddr: "my-dns-name:1051",
			want:          true,
		},
		{
			name:          "specified IP",
			advertiseAddr: "10.10.10.10:1051",
			want:          true,
		},
		{
			name:          "empty string",
			advertiseAddr: "",
			want:          false,
		},
		{
			name:          "not specified IP",
			advertiseAddr: "0.0.0.0:1051",
			want:          false,
		},
		{
			name:          "ipv4 wildcard string",
			advertiseAddr: ":1051",
			want:          false,
		},
		{
			name:          "ipv6 wildcard string",
			advertiseAddr: ":::1051",
			want:          false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{
				AdvertiseAddr: tt.advertiseAddr,
			}
			require.Equal(t, c.AdvertiseAddrIsSpecified(), tt.want)
		})
	}
}
