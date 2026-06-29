package gubernator

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	etcd "go.etcd.io/etcd/client/v3"
)

// TestNewEtcdPoolRunFailureReturnsNil ensures that NewEtcdPool returns a nil *EtcdPool when
// run() fails, so callers that assign the result to a PoolInterface cannot end up with a
// non-nil interface wrapping a nil pointer (which would panic on Close).
func TestNewEtcdPoolRunFailureReturnsNil(t *testing.T) {
	// Create a real client then close it immediately so register() fails fast
	// without waiting for a network timeout.
	client, err := etcd.New(etcd.Config{
		Endpoints: []string{"localhost:1"},
	})
	if !assert.NoError(t, err, "etcd.New should not fail before any dial attempt") {
		t.Skip("could not create etcd client; skipping")
	}
	client.Close()

	pool, err := NewEtcdPool(EtcdPoolConfig{
		Advertise: PeerInfo{GRPCAddress: "localhost:1051"},
		Client:    client,
	})
	require.Error(t, err)
	assert.Nil(t, pool, "NewEtcdPool must return nil on error to avoid typed-nil-in-interface panic")
}
