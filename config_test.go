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

func TestK8sConfigServiceName(t *testing.T) {
	os.Clearenv()
	s := `
GUBER_K8S_SERVICE_NAME=gubernator
GUBER_K8S_NAMESPACE=default
GUBER_K8S_POD_IP=10.0.0.1
GUBER_K8S_POD_PORT=9051`

	config, err := SetupDaemonConfig(logrus.StandardLogger(), strings.NewReader(s))
	require.NoError(t, err)
	require.Equal(t, "gubernator", config.K8PoolConf.ServiceName)
	require.Equal(t, "default", config.K8PoolConf.Namespace)
}

func TestK8sSelectorBackwardCompatibility(t *testing.T) {
	logger := logrus.New()
	logger.SetOutput(os.Stderr)

	t.Run("GUBER_K8S_SELECTOR takes precedence", func(t *testing.T) {
		os.Clearenv()
		s := `
GUBER_K8S_SELECTOR=app=gubernator-new
GUBER_K8S_ENDPOINTS_SELECTOR=app=gubernator-old
GUBER_K8S_WATCH_MECHANISM=pods`

		config, err := SetupDaemonConfig(logger, strings.NewReader(s))
		require.NoError(t, err)
		require.Equal(t, "app=gubernator-new", config.K8PoolConf.Selector)
	})

	t.Run("GUBER_K8S_ENDPOINTS_SELECTOR fallback", func(t *testing.T) {
		os.Clearenv()
		s := `
GUBER_K8S_ENDPOINTS_SELECTOR=app=gubernator
GUBER_K8S_WATCH_MECHANISM=pods`

		config, err := SetupDaemonConfig(logger, strings.NewReader(s))
		require.NoError(t, err)
		require.Equal(t, "app=gubernator", config.K8PoolConf.Selector)
	})

	t.Run("GUBER_K8S_SELECTOR preferred", func(t *testing.T) {
		os.Clearenv()
		s := `
GUBER_K8S_SELECTOR=app=gubernator
GUBER_K8S_WATCH_MECHANISM=pods`

		config, err := SetupDaemonConfig(logger, strings.NewReader(s))
		require.NoError(t, err)
		require.Equal(t, "app=gubernator", config.K8PoolConf.Selector)
	})
}

func TestK8sValidationEndpointSlices(t *testing.T) {
	os.Clearenv()

	t.Run("ServiceName required for endpointslices", func(t *testing.T) {
		os.Clearenv()
		s := `
GUBER_K8S_NAMESPACE=default
GUBER_K8S_POD_IP=10.0.0.1`

		_, err := SetupDaemonConfig(logrus.StandardLogger(), strings.NewReader(s))
		require.Error(t, err)
		require.ErrorContains(t, err, "GUBER_K8S_SERVICE_NAME")
	})

	t.Run("ServiceName provided for endpointslices succeeds", func(t *testing.T) {
		os.Clearenv()
		s := `
GUBER_K8S_SERVICE_NAME=gubernator
GUBER_K8S_NAMESPACE=default
GUBER_K8S_POD_IP=10.0.0.1
GUBER_K8S_POD_PORT=9051`

		config, err := SetupDaemonConfig(logrus.StandardLogger(), strings.NewReader(s))
		require.NoError(t, err)
		require.Equal(t, "gubernator", config.K8PoolConf.ServiceName)
		require.Equal(t, WatchEndpointSlices, config.K8PoolConf.Mechanism)
	})

	t.Run("Explicit endpointslices mechanism requires ServiceName", func(t *testing.T) {
		os.Clearenv()
		s := `
GUBER_K8S_WATCH_MECHANISM=endpointslices
GUBER_K8S_NAMESPACE=default`

		_, err := SetupDaemonConfig(logrus.StandardLogger(), strings.NewReader(s))
		require.Error(t, err)
		require.ErrorContains(t, err, "GUBER_K8S_SERVICE_NAME")
	})
}

func TestK8sValidationPods(t *testing.T) {
	os.Clearenv()

	t.Run("Selector required for pods mechanism", func(t *testing.T) {
		os.Clearenv()
		s := `
GUBER_K8S_WATCH_MECHANISM=pods
GUBER_K8S_NAMESPACE=default`

		_, err := SetupDaemonConfig(logrus.StandardLogger(), strings.NewReader(s))
		require.Error(t, err)
		require.ErrorContains(t, err, "GUBER_K8S_SELECTOR")
	})

	t.Run("Selector provided for pods succeeds", func(t *testing.T) {
		os.Clearenv()
		s := `
GUBER_K8S_WATCH_MECHANISM=pods
GUBER_K8S_SELECTOR=app=gubernator
GUBER_K8S_NAMESPACE=default`

		config, err := SetupDaemonConfig(logrus.StandardLogger(), strings.NewReader(s))
		require.NoError(t, err)
		require.Equal(t, "app=gubernator", config.K8PoolConf.Selector)
		require.Equal(t, WatchPods, config.K8PoolConf.Mechanism)
	})
}

func TestWatchMechanismErrorMessage(t *testing.T) {
	os.Clearenv()

	s := `
GUBER_K8S_WATCH_MECHANISM=invalid
GUBER_K8S_SERVICE_NAME=gubernator`

	_, err := SetupDaemonConfig(logrus.StandardLogger(), strings.NewReader(s))
	require.Error(t, err)
	require.ErrorContains(t, err, "endpointslices")
	require.ErrorContains(t, err, "pods")
}
