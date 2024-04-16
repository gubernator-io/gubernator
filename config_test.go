package gubernator

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func TestParsesAddress(t *testing.T) {
	os.Clearenv()
	s := `
# a comment
GUBER_HTTP_ADDRESS=10.10.10.10:9000`
	daemonConfig, err := SetupDaemonConfig(logrus.StandardLogger(), strings.NewReader(s))
	require.NoError(t, err)
	require.Equal(t, "10.10.10.10:9000", daemonConfig.HTTPListenAddress)
	require.NotEmpty(t, daemonConfig.InstanceID)
}

func TestDefaultListenAddress(t *testing.T) {
	os.Clearenv()
	s := `
# a comment`
	daemonConfig, err := SetupDaemonConfig(logrus.StandardLogger(), strings.NewReader(s))
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%s:80", LocalHost()), daemonConfig.HTTPListenAddress)
	require.NotEmpty(t, daemonConfig.InstanceID)
}

func TestDefaultInstanceId(t *testing.T) {
	os.Clearenv()
	s := ``
	daemonConfig, err := SetupDaemonConfig(logrus.StandardLogger(), strings.NewReader(s))
	require.NoError(t, err)
	require.NotEmpty(t, daemonConfig.InstanceID)
}
