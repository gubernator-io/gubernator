package gubernator

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
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

func TestEnvoyDefaults(t *testing.T) {
	os.Clearenv()
	conf, err := SetupDaemonConfig(logrus.StandardLogger(), strings.NewReader(""))
	require.NoError(t, err)
	assert.False(t, conf.Envoy.Enabled)
	assert.Equal(t, Algorithm_TOKEN_BUCKET, conf.Envoy.Algorithm)
	assert.Equal(t, Behavior_BATCHING, conf.Envoy.Behavior)
	assert.Equal(t, MissingLimitAction_DENY, conf.Envoy.OnMissingLimit)
	assert.Equal(t, 30*time.Second, conf.Envoy.PolicySyncInterval)
}

func TestEnvoyConfigFromEnv(t *testing.T) {
	os.Clearenv()
	s := `
GUBER_ENVOY_RLS_ENABLED=true
GUBER_ENVOY_ALGORITHM=LEAKY_BUCKET
GUBER_ENVOY_BEHAVIOR=GLOBAL,DURATION_IS_GREGORIAN
GUBER_ENVOY_ON_MISSING_LIMIT=allow
GUBER_ENVOY_POLICY_SYNC_INTERVAL=5s`
	conf, err := SetupDaemonConfig(logrus.StandardLogger(), strings.NewReader(s))
	require.NoError(t, err)
	assert.True(t, conf.Envoy.Enabled)
	assert.Equal(t, Algorithm_LEAKY_BUCKET, conf.Envoy.Algorithm)
	assert.Equal(t, Behavior_GLOBAL|Behavior_DURATION_IS_GREGORIAN, conf.Envoy.Behavior)
	assert.Equal(t, MissingLimitAction_ALLOW, conf.Envoy.OnMissingLimit)
	assert.Equal(t, 5*time.Second, conf.Envoy.PolicySyncInterval)
}

func TestEnvoyConfigRejectsUnknownValues(t *testing.T) {
	for _, test := range []struct {
		name    string
		env     string
		wantErr string
	}{
		{name: "Algorithm", env: "GUBER_ENVOY_ALGORITHM=SLIDING", wantErr: "GUBER_ENVOY_ALGORITHM=SLIDING"},
		{name: "Behavior", env: "GUBER_ENVOY_BEHAVIOR=GLOBAL,TURBO", wantErr: "GUBER_ENVOY_BEHAVIOR=TURBO"},
		{name: "OnMissingLimit", env: "GUBER_ENVOY_ON_MISSING_LIMIT=shrug", wantErr: "GUBER_ENVOY_ON_MISSING_LIMIT=shrug"},
	} {
		t.Run(test.name, func(t *testing.T) {
			os.Clearenv()
			_, err := SetupDaemonConfig(logrus.StandardLogger(), strings.NewReader(test.env))
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}
