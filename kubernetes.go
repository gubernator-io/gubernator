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
	"fmt"
	"reflect"

	"github.com/mailgun/holster/v4/setter"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	api_v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

type K8sPool struct {
	informer    cache.SharedIndexInformer
	client      *kubernetes.Clientset
	log         FieldLogger
	conf        K8sPoolConfig
	watchCtx    context.Context
	watchCancel func()
	done        chan struct{}
}

type WatchMechanism string

const (
	WatchEndpointSlices WatchMechanism = "endpointslices"
	WatchPods           WatchMechanism = "pods"
)

func WatchMechanismFromString(mechanism string) (WatchMechanism, error) {
	switch WatchMechanism(mechanism) {
	case "":
		return WatchEndpointSlices, nil
	case WatchEndpointSlices:
		return WatchEndpointSlices, nil
	case "endpoints": // backward compat - now uses EndpointSlices
		return WatchEndpointSlices, nil
	case WatchPods:
		return WatchPods, nil
	default:
		return "", fmt.Errorf("unknown watch mechanism specified: %s", mechanism)
	}
}

type K8sPoolConfig struct {
	Logger      FieldLogger
	Mechanism   WatchMechanism
	OnUpdate    UpdateFunc
	Namespace   string
	Selector    string // Label selector for pods mechanism (e.g., "app=gubernator")
	ServiceName string // Service name for endpointslices mechanism
	PodIP       string
	PodPort     string
}

func NewK8sPool(conf K8sPoolConfig) (*K8sPool, error) {
	config, err := RestConfig()
	if err != nil {
		return nil, errors.Wrap(err, "during InClusterConfig()")
	}
	// creates the client
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, errors.Wrap(err, "during NewForConfig()")
	}

	ctx, cancel := context.WithCancel(context.Background())

	pool := &K8sPool{
		done:        make(chan struct{}),
		log:         conf.Logger,
		client:      client,
		conf:        conf,
		watchCtx:    ctx,
		watchCancel: cancel,
	}
	setter.SetDefault(&pool.log, logrus.WithField("category", "gubernator"))

	return pool, pool.start()
}

func (e *K8sPool) start() error {
	switch e.conf.Mechanism {
	case WatchEndpointSlices:
		return e.startEndpointSliceWatch()
	case WatchPods:
		return e.startPodWatch()
	default:
		return fmt.Errorf("unknown value for watch mechanism: %s", e.conf.Mechanism)
	}
}

func (e *K8sPool) startGenericWatch(objType runtime.Object, listWatch *cache.ListWatch, updateFunc func()) error {
	e.informer = cache.NewSharedIndexInformer(
		listWatch,
		objType,
		0, //Skip resync
		cache.Indexers{},
	)

	_, err := e.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			e.log.Debugf("Queue (Add) '%s' - %v", key, err)
			if err != nil {
				e.log.Errorf("while calling MetaNamespaceKeyFunc(): %s", err)
				return
			}
			updateFunc()
		},
		UpdateFunc: func(obj, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			e.log.Debugf("Queue (Update) '%s' - %v", key, err)
			if err != nil {
				e.log.Errorf("while calling MetaNamespaceKeyFunc(): %s", err)
				return
			}
			updateFunc()
		},
		DeleteFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			e.log.Debugf("Queue (Delete) '%s' - %v", key, err)
			if err != nil {
				e.log.Errorf("while calling MetaNamespaceKeyFunc(): %s", err)
				return
			}
			updateFunc()
		},
	})
	if err != nil {
		e.log.WithError(err).Error("while adding event handler")
	}

	go e.informer.Run(e.done)

	if !cache.WaitForCacheSync(e.done, e.informer.HasSynced) {
		close(e.done)
		return fmt.Errorf("timed out waiting for caches to sync")
	}

	return nil
}

func (e *K8sPool) startPodWatch() error {
	listWatch := &cache.ListWatch{
		ListFunc: func(options meta_v1.ListOptions) (runtime.Object, error) {
			options.LabelSelector = e.conf.Selector
			return e.client.CoreV1().Pods(e.conf.Namespace).List(context.Background(), options)
		},
		WatchFunc: func(options meta_v1.ListOptions) (watch.Interface, error) {
			options.LabelSelector = e.conf.Selector
			return e.client.CoreV1().Pods(e.conf.Namespace).Watch(e.watchCtx, options)
		},
	}
	return e.startGenericWatch(&api_v1.Pod{}, listWatch, e.updatePeersFromPods)
}

func (e *K8sPool) startEndpointSliceWatch() error {
	listWatch := &cache.ListWatch{
		ListFunc: func(options meta_v1.ListOptions) (runtime.Object, error) {
			options.LabelSelector = discoveryv1.LabelServiceName + "=" + e.conf.ServiceName
			return e.client.DiscoveryV1().EndpointSlices(e.conf.Namespace).List(context.Background(), options)
		},
		WatchFunc: func(options meta_v1.ListOptions) (watch.Interface, error) {
			options.LabelSelector = discoveryv1.LabelServiceName + "=" + e.conf.ServiceName
			return e.client.DiscoveryV1().EndpointSlices(e.conf.Namespace).Watch(e.watchCtx, options)
		},
	}
	return e.startGenericWatch(&discoveryv1.EndpointSlice{}, listWatch, e.updatePeersFromEndpointSlices)
}

func (e *K8sPool) updatePeersFromPods() {
	e.log.Debug("Fetching peer list from pods API")

	var pods []*api_v1.Pod
	for _, obj := range e.informer.GetStore().List() {
		pod, ok := obj.(*api_v1.Pod)
		if !ok {
			e.log.Errorf("expected type v1.Pod got '%s' instead", reflect.TypeOf(obj).String())
			continue
		}
		pods = append(pods, pod)
	}

	peers := ExtractPeersFromPods(pods, e.conf.PodIP, e.conf.PodPort, e.log)
	e.conf.OnUpdate(peers)
}

// ExtractPeersFromPods converts Pod objects to a PeerInfo slice.
// This is a pure function that extracts peer information from Kubernetes Pods.
func ExtractPeersFromPods(pods []*api_v1.Pod, podIP, podPort string, log FieldLogger) []PeerInfo {
	var peers []PeerInfo

	for _, pod := range pods {
		peer := PeerInfo{GRPCAddress: fmt.Sprintf("%s:%s", pod.Status.PodIP, podPort)}
		if pod.Status.PodIP == podIP {
			peer.IsOwner = true
		}

		// Only take the chance to skip the peer if it's not our own IP. We need to be able to discover ourselves
		// for the health check.
		if !peer.IsOwner {
			skip := false
			// if containers are not ready or not running then skip this peer
			for _, status := range pod.Status.ContainerStatuses {
				if !status.Ready || status.State.Running == nil {
					log.Debugf("Skipping peer because it's not ready or not running: %+v\n", peer)
					skip = true
					break
				}
			}
			if skip {
				continue
			}
		}

		log.Debugf("Peer: %+v\n", peer)
		peers = append(peers, peer)
	}

	return peers
}

func (e *K8sPool) updatePeersFromEndpointSlices() {
	e.log.Debug("Fetching peer list from endpointslices API")

	var slices []*discoveryv1.EndpointSlice
	for _, obj := range e.informer.GetStore().List() {
		slice, ok := obj.(*discoveryv1.EndpointSlice)
		if !ok {
			e.log.Errorf("expected type discoveryv1.EndpointSlice got '%s' instead", reflect.TypeOf(obj).String())
			continue
		}
		slices = append(slices, slice)
	}

	peers := ExtractPeersFromEndpointSlices(slices, e.conf.PodIP, e.conf.PodPort, e.log)
	e.conf.OnUpdate(peers)
}

// ExtractPeersFromEndpointSlices converts EndpointSlice objects to a PeerInfo slice.
// This is a pure function that extracts peer information from Kubernetes EndpointSlices.
func ExtractPeersFromEndpointSlices(slices []*discoveryv1.EndpointSlice, podIP, podPort string, log FieldLogger) []PeerInfo {
	peerMap := make(map[string]PeerInfo)

	for _, slice := range slices {
		if slice.AddressType != discoveryv1.AddressTypeIPv4 {
			continue
		}

		for _, endpoint := range slice.Endpoints {
			if len(endpoint.Addresses) == 0 {
				continue
			}

			ip := endpoint.Addresses[0]

			isReady := endpoint.Conditions.Ready == nil || *endpoint.Conditions.Ready

			isOwner := ip == podIP

			if !isReady && !isOwner {
				log.Debugf("Skipping peer because it's not ready: %s", ip)
				continue
			}

			peer := PeerInfo{
				GRPCAddress: fmt.Sprintf("%s:%s", ip, podPort),
				IsOwner:     isOwner,
			}

			if existing, exists := peerMap[ip]; exists {
				if !existing.IsOwner && isOwner {
					peerMap[ip] = peer
				}
				continue
			}

			peerMap[ip] = peer
			log.Debugf("Peer: %+v", peer)
		}
	}

	peers := make([]PeerInfo, 0, len(peerMap))
	for _, peer := range peerMap {
		peers = append(peers, peer)
	}

	return peers
}

func (e *K8sPool) Close() {
	e.watchCancel()
	close(e.done)
}
