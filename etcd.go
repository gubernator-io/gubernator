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

package gubernator

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/kapetan-io/errors"
	"github.com/kapetan-io/tackle/clock"
	"github.com/kapetan-io/tackle/set"
	"github.com/kapetan-io/tackle/wait"
	etcd "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/grpclog"
)

const (
	etcdTimeout       = clock.Second * 10
	backOffTimeout    = clock.Second * 5
	leaseTTL          = 30
	defaultBaseKey    = "/gubernator/peers/"
	localEtcdEndpoint = "127.0.0.1:2379"
)

type PoolInterface interface {
	Close()
}

type EtcdPool struct {
	peers     map[string]PeerInfo
	wg        wait.Group
	ctx       context.Context
	cancelCtx context.CancelFunc
	watchChan etcd.WatchChan
	log       FieldLogger
	watcher   etcd.Watcher
	conf      EtcdPoolConfig
}

type EtcdPoolConfig struct {
	// (Required) This is the peer information that will be advertised to other gubernator instances
	Advertise PeerInfo

	// (Required) An etcd client currently connected to an etcd cluster
	Client *etcd.Client

	// (Required) Called when the list of gubernators in the pool updates
	OnUpdate UpdateFunc

	// (Optional) The etcd key prefix used when discovering other peers. Defaults to `/gubernator/peers/`
	KeyPrefix string

	// (Optional) The etcd config used to connect to the etcd cluster
	EtcdConfig *etcd.Config

	// (Optional) An interface through which logging will occur (Usually *logrus.Entry)
	Logger FieldLogger
}

func NewEtcdPool(conf EtcdPoolConfig) (*EtcdPool, error) {
	set.Default(&conf.KeyPrefix, defaultBaseKey)
	set.Default(&conf.Logger, slog.Default().With("category", "gubernator"))

	if conf.Advertise.HTTPAddress == "" {
		return nil, errors.New("Advertise.HTTPAddress is required")
	}

	if conf.Client == nil {
		return nil, errors.New("Client is required")
	}

	ctx, cancel := context.WithCancel(context.Background())
	pool := &EtcdPool{
		log:       conf.Logger,
		peers:     make(map[string]PeerInfo),
		cancelCtx: cancel,
		conf:      conf,
		ctx:       ctx,
	}
	return pool, pool.run(conf.Advertise)
}

func (e *EtcdPool) run(peer PeerInfo) error {
	// Register our instance with etcd
	if err := e.register(peer); err != nil {
		return err
	}

	// Get our peer list and watch for changes
	if err := e.watch(); err != nil {
		return err
	}
	return nil
}

func (e *EtcdPool) watchPeers() error {
	var revision int64

	// Update our list of peers
	if err := e.collectPeers(&revision); err != nil {
		return err
	}

	// Cancel any previous watches
	if e.watcher != nil {
		e.watcher.Close()
	}

	e.watcher = etcd.NewWatcher(e.conf.Client)

	ready := make(chan struct{})
	go func() {
		e.watchChan = e.watcher.Watch(etcd.WithRequireLeader(e.ctx), e.conf.KeyPrefix,
			etcd.WithRev(revision), etcd.WithPrefix(), etcd.WithPrevKV())
		close(ready)
	}()

	select {
	case <-ready:
		e.log.LogAttrs(context.TODO(), slog.LevelInfo, "watching for peer changes",
			slog.String("key_prefix", e.conf.KeyPrefix),
			slog.Int64("revision", revision),
		)
	case <-clock.After(etcdTimeout):
		return errors.New("timed out while waiting for watcher.Watch() to start")
	}
	return nil
}

func (e *EtcdPool) collectPeers(revision *int64) error {
	ctx, cancel := context.WithTimeout(e.ctx, etcdTimeout)
	defer cancel()

	resp, err := e.conf.Client.Get(ctx, e.conf.KeyPrefix, etcd.WithPrefix())
	if err != nil {
		return errors.Errorf("while fetching peer listing from '%s': %w", e.conf.KeyPrefix, err)
	}

	peers := make(map[string]PeerInfo)
	// Collect all the peers
	for _, v := range resp.Kvs {
		p := e.unMarshallValue(v.Value)
		peers[p.HTTPAddress] = p
	}

	e.peers = peers
	*revision = resp.Header.Revision
	e.callOnUpdate()
	return nil
}

func (e *EtcdPool) unMarshallValue(v []byte) PeerInfo {
	var p PeerInfo

	// for backward compatible with older gubernator versions
	if err := json.Unmarshal(v, &p); err != nil {
		e.log.LogAttrs(context.TODO(), slog.LevelError, "while unmarshalling peer info from key value",
			ErrAttr(err),
		)
		return PeerInfo{HTTPAddress: string(v)}
	}
	return p
}

func (e *EtcdPool) watch() error {
	var rev int64

	// Initialize watcher
	if err := e.watchPeers(); err != nil {
		return errors.Errorf("while attempting to start watch: %w", err)
	}

	e.wg.Until(func(done chan struct{}) bool {
		for response := range e.watchChan {
			if response.Canceled {
				e.log.Info("graceful watch shutdown")
				return false
			}

			if err := response.Err(); err != nil {
				e.log.LogAttrs(context.TODO(), slog.LevelError, "watch error",
					ErrAttr(err),
				)
				goto restart
			}
			_ = e.collectPeers(&rev)
		}

	restart:
		// Are we in the middle of a shutdown?
		select {
		case <-done:
			return false
		case <-e.ctx.Done():
			return false
		default:
		}

		if err := e.watchPeers(); err != nil {
			e.log.LogAttrs(context.TODO(), slog.LevelError, "while attempting to restart watch",
				ErrAttr(err),
			)
			select {
			case <-clock.After(backOffTimeout):
				return true
			case <-done:
				return false
			}
		}

		return true
	})
	return nil
}

func (e *EtcdPool) register(peer PeerInfo) error {
	instanceKey := e.conf.KeyPrefix + peer.HTTPAddress
	e.log.LogAttrs(context.TODO(), slog.LevelInfo, "Registering peer with etcd",
		slog.Any("peer", peer),
	)

	b, err := json.Marshal(peer)
	if err != nil {
		return errors.Errorf("while marshalling PeerInfo: %w", err)
	}

	var keepAlive <-chan *etcd.LeaseKeepAliveResponse
	var lease *etcd.LeaseGrantResponse

	register := func() error {
		ctx, cancel := context.WithTimeout(e.ctx, etcdTimeout)
		defer cancel()
		var err error

		lease, err = e.conf.Client.Grant(ctx, leaseTTL)
		if err != nil {
			return errors.Errorf("during grant lease: %w", err)
		}

		_, err = e.conf.Client.Put(ctx, instanceKey, string(b), etcd.WithLease(lease.ID))
		if err != nil {
			return errors.Errorf("during put: %w", err)
		}

		if keepAlive, err = e.conf.Client.KeepAlive(e.ctx, lease.ID); err != nil {
			return err
		}
		return nil
	}

	var lastKeepAlive clock.Time

	// Attempt to register our instance with etcd
	if err = register(); err != nil {
		return errors.Errorf("during initial peer registration: %w", err)
	}

	e.wg.Until(func(done chan struct{}) bool {
		// If we have lost our keep alive, register again
		if keepAlive == nil {
			if err = register(); err != nil {
				e.log.LogAttrs(context.TODO(), slog.LevelError, "while attempting to re-register peer",
					ErrAttr(err),
				)
				select {
				case <-clock.After(backOffTimeout):
					return true
				case <-done:
					return false
				}
			}
		}

		select {
		case _, ok := <-keepAlive:
			if !ok {
				// Don't re-register if we are in the middle of a shutdown
				if e.ctx.Err() != nil {
					return true
				}

				e.log.Warn("keep alive lost, attempting to re-register peer")
				// re-register
				keepAlive = nil
				return true
			}

			// Ensure we are getting keep alive's regularly
			if lastKeepAlive.Sub(clock.Now()) > clock.Second*leaseTTL {
				e.log.Warn("to long between keep alive heartbeats, re-registering peer")
				keepAlive = nil
				return true
			}
			lastKeepAlive = clock.Now()
		case <-done:
			ctx, cancel := context.WithTimeout(context.Background(), etcdTimeout)
			if _, err := e.conf.Client.Delete(ctx, instanceKey); err != nil {
				e.log.LogAttrs(context.TODO(), slog.LevelError, "during etcd delete",
					ErrAttr(err),
				)
			}

			if _, err := e.conf.Client.Revoke(ctx, lease.ID); err != nil {
				e.log.LogAttrs(context.TODO(), slog.LevelError, "during lease revoke",
					ErrAttr(err),
				)
			}
			cancel()
			return false
		}
		return true
	})

	return nil
}

func (e *EtcdPool) Close() {
	e.cancelCtx()
	e.wg.Stop()
}

func (e *EtcdPool) callOnUpdate() {
	var peers []PeerInfo

	for _, p := range e.peers {
		if p.HTTPAddress == e.conf.Advertise.HTTPAddress {
			p.IsOwner = true
		}
		peers = append(peers, p)
	}

	e.conf.OnUpdate(peers)
}

// GetPeers returns a list of peers from etcd.
func (e *EtcdPool) GetPeers(ctx context.Context) ([]PeerInfo, error) {
	keyPrefix := e.conf.KeyPrefix

	resp, err := e.conf.Client.Get(ctx, keyPrefix, etcd.WithPrefix())
	if err != nil {
		return nil, errors.Errorf("while fetching peer listing from '%s': %w", keyPrefix, err)
	}

	var peers []PeerInfo

	for _, v := range resp.Kvs {
		p := e.unMarshallValue(v.Value)
		peers = append(peers, p)
	}

	return peers, nil
}

func init() {
	// We check this here to avoid data race with GRPC go routines writing to the logger
	if os.Getenv("ETCD3_DEBUG") != "" {
		grpclog.SetLoggerV2(grpclog.NewLoggerV2WithVerbosity(os.Stderr, os.Stderr, os.Stderr, 4))
	}
}

// newEtcdClient creates a new etcd.Client with the specified config where blanks
// are filled from environment variables by NewConfig.
//
// If the provided config is nil and no environment variables are set, it will
// return a client connecting without TLS via localhost:2379.
func newEtcdClient(cfg *etcd.Config) (*etcd.Client, error) {
	var err error
	if cfg, err = newConfig(cfg); err != nil {
		return nil, fmt.Errorf("failed to build etcd config: %w", err)
	}

	etcdClt, err := etcd.New(*cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create etcd client: %w", err)
	}
	return etcdClt, nil
}

// NewConfig creates a new etcd.Config using environment variables. If an
// existing config is passed, it will fill in missing configuration using
// environment variables or defaults if they exists on the local system.
//
// If no environment variables are set, it will return a config set to
// connect without TLS via localhost:2379.
func newConfig(cfg *etcd.Config) (*etcd.Config, error) {
	var envEndpoint, tlsCertFile, tlsKeyFile, tlsCAFile string

	set.Default(&cfg, &etcd.Config{})
	set.Default(&cfg.Username, os.Getenv("ETCD3_USER"))
	set.Default(&cfg.Password, os.Getenv("ETCD3_PASSWORD"))
	set.Default(&tlsCertFile, os.Getenv("ETCD3_TLS_CERT"))
	set.Default(&tlsKeyFile, os.Getenv("ETCD3_TLS_KEY"))
	set.Default(&tlsCAFile, os.Getenv("ETCD3_CA"))

	// Default to 5 second timeout, else connections hang indefinitely
	set.Default(&cfg.DialTimeout, clock.Second*5)
	// Or if the user provided a timeout
	if timeout := os.Getenv("ETCD3_DIAL_TIMEOUT"); timeout != "" {
		duration, err := clock.ParseDuration(timeout)
		if err != nil {
			return nil, errors.Errorf(
				"ETCD3_DIAL_TIMEOUT='%s' is not a duration (1m|15s|24h): %s", timeout, err)
		}
		cfg.DialTimeout = duration
	}

	defaultCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	// If the CA file was provided
	if tlsCAFile != "" {
		set.Default(&cfg.TLS, defaultCfg)

		var certPool *x509.CertPool = nil
		if pemBytes, err := os.ReadFile(tlsCAFile); err == nil {
			certPool = x509.NewCertPool()
			certPool.AppendCertsFromPEM(pemBytes)
		} else {
			return nil, errors.Errorf("while loading cert CA file '%s': %s", tlsCAFile, err)
		}
		set.Default(&cfg.TLS.RootCAs, certPool)
		cfg.TLS.InsecureSkipVerify = false
	}

	// If the cert and key files are provided attempt to load them
	if tlsCertFile != "" && tlsKeyFile != "" {
		set.Default(&cfg.TLS, defaultCfg)
		tlsCert, err := tls.LoadX509KeyPair(tlsCertFile, tlsKeyFile)
		if err != nil {
			return nil, errors.Errorf("while loading cert '%s' and key file '%s': %s",
				tlsCertFile, tlsKeyFile, err)
		}
		set.Default(&cfg.TLS.Certificates, []tls.Certificate{tlsCert})
	}

	set.Default(&envEndpoint, os.Getenv("ETCD3_ENDPOINT"), localEtcdEndpoint)
	set.Default(&cfg.Endpoints, strings.Split(envEndpoint, ","))

	// If no other TLS config is provided this will force connecting with TLS,
	// without cert verification
	if os.Getenv("ETCD3_SKIP_VERIFY") != "" {
		set.Default(&cfg.TLS, defaultCfg)
		cfg.TLS.InsecureSkipVerify = true
	}

	// Enable TLS with no additional configuration
	if os.Getenv("ETCD3_ENABLE_TLS") != "" {
		set.Default(&cfg.TLS, defaultCfg)
	}

	return cfg, nil
}
