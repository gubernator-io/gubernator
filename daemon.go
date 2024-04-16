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
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mailgun/holster/v4/errors"
	"github.com/mailgun/holster/v4/etcdutil"
	"github.com/mailgun/holster/v4/setter"
	"github.com/mailgun/holster/v4/syncutil"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

type Daemon struct {
	wg             syncutil.WaitGroup
	httpServers    []*http.Server
	pool           PoolInterface
	conf           DaemonConfig
	Listener       net.Listener
	HealthListener net.Listener
	log            FieldLogger
	PeerInfo       PeerInfo
	Service        *Service
	InstanceID     string
	logAdaptor     io.WriteCloser
}

// SpawnDaemon starts a new gubernator daemon according to the provided DaemonConfig.
// This function will block until the daemon responds to connections to HTTPListenAddress
func SpawnDaemon(ctx context.Context, conf DaemonConfig) (*Daemon, error) {
	setter.SetDefault(&conf.Logger, logrus.WithFields(logrus.Fields{
		"service-id": conf.InstanceID,
		"category":   "gubernator",
	}))

	s := &Daemon{
		logAdaptor: newLogAdaptor(conf.Logger),
		InstanceID: conf.InstanceID,
		log:        conf.Logger,
		conf:       conf,
	}
	return s, s.Start(ctx)
}

func (d *Daemon) Start(ctx context.Context) error {
	var err error

	registry := prometheus.NewRegistry()

	// The LRU cache for storing rate limits.
	cacheCollector := NewLRUCacheCollector()
	registry.MustRegister(cacheCollector)

	if err := SetupTLS(d.conf.TLS); err != nil {
		return err
	}

	d.Service, err = NewService(Config{
		PeerClientFactory: func(info PeerInfo) PeerClient {
			return NewPeerClient(WithPeerInfo(info))
		},
		CacheFactory: func(maxSize int) Cache {
			cache := NewLRUCache(maxSize)
			cacheCollector.AddCache(cache)
			return cache
		},
		DataCenter:  d.conf.DataCenter,
		InstanceID:  d.conf.InstanceID,
		CacheSize:   d.conf.CacheSize,
		Behaviors:   d.conf.Behaviors,
		Workers:     d.conf.Workers,
		LocalPicker: d.conf.Picker,
		Loader:      d.conf.Loader,
		Store:       d.conf.Store,
		Logger:      d.log,
	})
	if err != nil {
		return errors.Wrap(err, "while creating new gubernator service")
	}

	// Service implements prometheus.Collector interface
	registry.MustRegister(d.Service)

	switch d.conf.PeerDiscoveryType {
	case "k8s":
		// Source our list of peers from kubernetes endpoint API
		d.conf.K8PoolConf.OnUpdate = d.Service.SetPeers
		d.pool, err = NewK8sPool(d.conf.K8PoolConf)
		if err != nil {
			return errors.Wrap(err, "while querying kubernetes API")
		}
	case "etcd":
		d.conf.EtcdPoolConf.OnUpdate = d.Service.SetPeers
		// Register ourselves with other peers via ETCD
		d.conf.EtcdPoolConf.Client, err = etcdutil.NewClient(d.conf.EtcdPoolConf.EtcdConfig)
		if err != nil {
			return errors.Wrap(err, "while connecting to etcd")
		}

		d.pool, err = NewEtcdPool(d.conf.EtcdPoolConf)
		if err != nil {
			return errors.Wrap(err, "while creating etcd pool")
		}
	case "dns":
		d.conf.DNSPoolConf.OnUpdate = d.Service.SetPeers
		d.pool, err = NewDNSPool(d.conf.DNSPoolConf)
		if err != nil {
			return errors.Wrap(err, "while creating the DNS pool")
		}
	case "member-list":
		d.conf.MemberListPoolConf.OnUpdate = d.Service.SetPeers
		d.conf.MemberListPoolConf.Logger = d.log

		// Register peer on the member list
		d.pool, err = NewMemberListPool(ctx, d.conf.MemberListPoolConf)
		if err != nil {
			return errors.Wrap(err, "while creating member list pool")
		}
	}

	// Optionally collect process metrics
	if d.conf.MetricFlags.Has(FlagOSMetrics) {
		d.log.Debug("Collecting OS Metrics")
		registry.MustRegister(collectors.NewProcessCollector(
			collectors.ProcessCollectorOpts{Namespace: "gubernator"},
		))
	}

	// Optionally collect golang internal metrics
	if d.conf.MetricFlags.Has(FlagGolangMetrics) {
		d.log.Debug("Collecting Golang Metrics")
		registry.MustRegister(collectors.NewGoCollector())
	}

	handler := NewHandler(d.Service, promhttp.InstrumentMetricHandler(
		registry, promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
	))
	registry.MustRegister(handler)

	if d.conf.ServerTLS() != nil {
		if err := d.spawnHTTPS(ctx, handler); err != nil {
			return err
		}
		if d.conf.HTTPStatusListenAddress != "" {
			if err := d.spawnHTTPHealthCheck(ctx, handler, registry); err != nil {
				return err
			}
		}
	} else {
		if err := d.spawnHTTP(ctx, handler); err != nil {
			return err
		}
	}

	d.PeerInfo = PeerInfo{
		HTTPAddress: d.Listener.Addr().String(),
		DataCenter:  d.conf.DataCenter,
	}

	return nil
}

// spawnHTTPHealthCheck spawns a plan HTTP listener for use by orchestration systems to preform health checks and
// collect metrics when TLS and client certs are in use.
func (d *Daemon) spawnHTTPHealthCheck(ctx context.Context, h *Handler, r *prometheus.Registry) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.HealthZ)
	mux.Handle("/metrics", promhttp.InstrumentMetricHandler(
		r, promhttp.HandlerFor(r, promhttp.HandlerOpts{}),
	))
	srv := &http.Server{
		ErrorLog:  log.New(d.logAdaptor, "", 0),
		Addr:      d.conf.HTTPStatusListenAddress,
		TLSConfig: d.conf.ServerTLS().Clone(),
		Handler:   mux,
	}

	srv.TLSConfig.ClientAuth = tls.NoClientCert
	var err error
	d.HealthListener, err = net.Listen("tcp", d.conf.HTTPStatusListenAddress)
	if err != nil {
		return errors.Wrap(err, "while starting HTTP listener for health metric")
	}

	d.wg.Go(func() {
		d.log.Infof("HTTPS Health Check Listening on %s ...", d.conf.HTTPStatusListenAddress)
		if err := srv.ServeTLS(d.HealthListener, "", ""); err != nil {
			if !errors.Is(err, http.ErrServerClosed) {
				d.log.WithError(err).Error("while starting TLS Status HTTP server")
			}
		}
	})

	if err := WaitForConnect(ctx, d.HealthListener.Addr().String(), nil); err != nil {
		return err
	}

	d.httpServers = append(d.httpServers, srv)
	return nil
}

func (d *Daemon) spawnHTTPS(ctx context.Context, mux http.Handler) error {
	srv := &http.Server{
		ErrorLog:  log.New(d.logAdaptor, "", 0),
		TLSConfig: d.conf.ServerTLS().Clone(),
		Addr:      d.conf.HTTPListenAddress,
		Handler:   mux,
	}

	var err error
	d.Listener, err = net.Listen("tcp", d.conf.HTTPListenAddress)
	if err != nil {
		return errors.Wrap(err, "while starting HTTPS listener")
	}

	d.wg.Go(func() {
		d.log.Infof("HTTPS Listening on %s ...", d.conf.HTTPListenAddress)
		if err := srv.ServeTLS(d.Listener, "", ""); err != nil {
			if !errors.Is(err, http.ErrServerClosed) {
				d.log.WithError(err).Error("while starting TLS HTTP server")
			}

		}
	})
	if err := WaitForConnect(ctx, d.Listener.Addr().String(), d.conf.ClientTLS()); err != nil {
		return err
	}

	d.httpServers = append(d.httpServers, srv)

	return nil
}

func (d *Daemon) spawnHTTP(ctx context.Context, h http.Handler) error {
	srv := &http.Server{
		ErrorLog: log.New(d.logAdaptor, "", 0),
		Addr:     d.conf.HTTPListenAddress,
		Handler:  h,
	}
	var err error
	d.Listener, err = net.Listen("tcp", d.conf.HTTPListenAddress)
	if err != nil {
		return errors.Wrap(err, "while starting HTTP listener")
	}

	d.wg.Go(func() {
		d.log.Infof("HTTP Listening on %s ...", d.conf.HTTPListenAddress)
		if err := srv.Serve(d.Listener); err != nil {
			if !errors.Is(err, http.ErrServerClosed) {
				d.log.WithError(err).Error("while starting HTTP server")
			}
		}
	})

	if err := WaitForConnect(ctx, d.Listener.Addr().String(), nil); err != nil {
		return err
	}

	d.httpServers = append(d.httpServers, srv)
	return nil
}

// Close gracefully closes all server connections and listening sockets
func (d *Daemon) Close(ctx context.Context) error {
	if len(d.httpServers) == 0 {
		return nil
	}

	for _, srv := range d.httpServers {
		d.log.Infof("Shutting down server %s ...", srv.Addr)
		_ = srv.Shutdown(ctx)
	}
	d.httpServers = nil

	if err := d.Service.Close(ctx); err != nil {
		return err
	}
	d.Service = nil

	_ = d.logAdaptor.Close()
	d.HealthListener = nil
	d.Listener = nil

	d.wg.Wait()
	return nil
}

// SetPeers sets the peers for this daemon
func (d *Daemon) SetPeers(in []PeerInfo) {
	peers := make([]PeerInfo, len(in))
	copy(peers, in)

	for i, p := range peers {
		peers[i].SetTLS(d.conf.ClientTLS())
		if d.conf.AdvertiseAddress == p.HTTPAddress {
			peers[i].IsOwner = true
		}
	}
	d.Service.SetPeers(peers)
}

// Config returns the current config for this Daemon
func (d *Daemon) Config() DaemonConfig {
	return d.conf
}

// Peers returns the peers this daemon knows about
func (d *Daemon) Peers() []PeerInfo {
	var peers []PeerInfo
	for _, client := range d.Service.GetPeerList() {
		peers = append(peers, client.Info())
	}
	return peers
}

func (d *Daemon) MustClient() Client {
	c, err := d.Client()
	if err != nil {
		panic(fmt.Sprintf("[%s] failed to init daemon client - '%d'", d.InstanceID, err))
	}
	return c
}

func (d *Daemon) Client() (Client, error) {
	if d.conf.TLS != nil {
		return NewClient(WithTLS(d.conf.ClientTLS(), d.Listener.Addr().String()))
	}
	return NewClient(WithNoTLS(d.Listener.Addr().String()))
}

// WaitForConnect waits until the passed address is accepting connections.
// It will continue to attempt a connection until context is canceled.
func WaitForConnect(ctx context.Context, address string, cfg *tls.Config) error {
	if address == "" {
		return fmt.Errorf("WaitForConnect() requires a valid address")
	}

	var errs []string
	for {
		var d proxy.ContextDialer
		if cfg != nil {
			d = &tls.Dialer{Config: cfg}
		} else {
			d = &net.Dialer{}
		}
		conn, err := d.DialContext(ctx, "tcp", address)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		errs = append(errs, err.Error())
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err().Error())
			return errors.New(strings.Join(errs, "\n"))
		}
		time.Sleep(time.Millisecond * 100)
		continue
	}
}
