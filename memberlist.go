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
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/gob"
	"encoding/json"
	"io"
	"net"
	"runtime"
	"strconv"
	"strings"

	ml "github.com/hashicorp/memberlist"
	"github.com/mailgun/holster/v4/clock"
	"github.com/mailgun/holster/v4/retry"
	"github.com/mailgun/holster/v4/setter"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type MemberListPool struct {
	log        FieldLogger
	memberList *ml.Memberlist
	conf       MemberListPoolConfig
	events     *memberListEventHandler
}

type MemberListPoolConfig struct {
	// (Required) This is the peer information that will be advertised to other members
	Advertise PeerInfo

	// (Optional) The bind MemberListBindAddress that will listen for the memberlist gossip.
	// Default is 0.0.0.0:7946 in memberlist package.
	MemberListBindAddress string

	// (Required) This is the address:port the member list protocol listen for other members on
	MemberListAddress string

	// (Required) This is the address:port the member list will advertise to other members it finds
	AdvertiseAddress string

	// (Required) A list of nodes this member list instance can contact to find other members.
	KnownNodes []string

	// (Required) A callback function which is called when the member list changes
	OnUpdate UpdateFunc

	// (Optional) The name of the node this member list identifies itself as.
	NodeName string

	// (Optional) An interface through which logging will occur (Usually *logrus.Entry)
	Logger FieldLogger

	// (Optional) The encryption settings used for memberlist.
	EncryptionConfig MemberListEncryptionConfig
}

type MemberListEncryptionConfig struct {
	// (Required) A list of base64 encoded keys. Each key should be either 16, 24, or 32 bytes
	// when decoded to select AES-128, AES-192, or AES-256 respectively.
	// The first key in the list will be used for encrypting outbound messages. All keys are
	// attempted when decrypting gossip, which allows for rotations.
	SecretKeys []string `json:"secret-keys"`
	// (Optional) Defaults to true. Controls whether to enforce encryption for incoming
	// gossip. It is used for upshifting from unencrypted to encrypted gossip on
	// a running cluster.
	GossipVerifyIncoming bool `json:"gossip-verify-incoming"`
	// (Optional) Defaults to true. Controls whether to enforce encryption for outgoing
	// gossip. It is used for upshifting from unencrypted to encrypted gossip on
	// a running cluster.
	GossipVerifyOutgoing bool `json:"gossip-verify-outgoing"`
}

func NewMemberListPool(ctx context.Context, conf MemberListPoolConfig) (*MemberListPool, error) {
	setter.SetDefault(conf.Logger, logrus.WithField("category", "gubernator"))
	m := &MemberListPool{
		log:  conf.Logger,
		conf: conf,
	}

	host, port, err := splitAddress(conf.MemberListAddress)
	if err != nil {
		return nil, errors.Wrap(err, "MemberListAddress=`%s` is invalid;")
	}

	// Member list requires the address to be an ip address
	if ip := net.ParseIP(host); ip == nil {
		addrs, err := net.LookupHost(host)
		if err != nil {
			return nil, errors.Wrapf(err, "while preforming host lookup for '%s'", host)
		}
		if len(addrs) == 0 {
			return nil, errors.Wrapf(err, "net.LookupHost() returned no addresses for '%s'", host)
		}
		host = addrs[0]
	}

	// Configure member list event handler
	m.events = newMemberListEventHandler(m.log, conf)

	// Configure member list
	config := ml.DefaultWANConfig()
	config.Events = m.events
	config.AdvertiseAddr = host
	config.AdvertisePort = port

	// Configure the memberlist bind address and port if MemberListBindAddress
	// is not empty
	if conf.MemberListBindAddress != "" {
		bindHost, bindPort, err := splitAddress(conf.MemberListBindAddress)
		if err != nil {
			return nil, errors.Wrap(err, "MemberListBindAddress=`%s` is invalid;")
		}
		// Member list requires the address to be an ip address
		if ip := net.ParseIP(bindHost); ip == nil {
			addrs, err := net.LookupHost(bindHost)
			if err != nil {
				return nil, errors.Wrapf(err, "while preforming bind host lookup for '%s'", bindHost)
			}
			if len(addrs) == 0 {
				return nil, errors.Wrapf(err, "net.LookupHost() returned no bind addresses for '%s'", bindHost)
			}
			bindHost = addrs[0]
		}
		config.BindAddr = bindHost
		config.BindPort = bindPort
	}

	// Enable gossip encryption if a key is defined.
	if len(conf.EncryptionConfig.SecretKeys) > 0 {
		config.GossipVerifyIncoming = conf.EncryptionConfig.GossipVerifyIncoming
		config.GossipVerifyOutgoing = conf.EncryptionConfig.GossipVerifyOutgoing
		config.Keyring = &ml.Keyring{}
		for i, key := range conf.EncryptionConfig.SecretKeys {
			secret, err := base64.StdEncoding.DecodeString(strings.TrimSpace(key))
			if err != nil {
				return nil, errors.Wrapf(err, "unable to base64 decode memberlist encryption key at index %d", i)
			}
			err = config.Keyring.AddKey(secret)
			if err != nil {
				return nil, errors.Wrapf(err, "unable to add secret key at index %d to memberlist", i)
			}
			// Set the first key as the default for encrypting outbound messages.
			if i == 0 {
				config.SecretKey = secret
			}
		}
	}

	if conf.NodeName != "" {
		config.Name = conf.NodeName
	}

	config.LogOutput = newLogWriter(m.log)

	// Create and set member list
	memberList, err := ml.Create(config)
	if err != nil {
		return nil, err
	}
	m.memberList = memberList

	// Prep metadata
	gob.Register(memberListMetadata{})

	// Join member list pool
	err = m.joinPool(ctx, conf)
	if err != nil {
		return nil, errors.Wrap(err, "while attempting to join the member-list pool")
	}

	return m, nil
}

func (m *MemberListPool) joinPool(ctx context.Context, conf MemberListPoolConfig) error {
	// Get local node and set metadata
	node := m.memberList.LocalNode()
	b, err := json.Marshal(&conf.Advertise)
	if err != nil {
		return errors.Wrap(err, "error marshalling PeerInfo as JSON")
	}
	node.Meta = b

	err = retry.Until(ctx, retry.Interval(clock.Millisecond*300), func(ctx context.Context, i int) error {
		// Join member list
		_, err = m.memberList.Join(conf.KnownNodes)
		if err != nil {
			return errors.Wrap(err, "while joining member-list")
		}
		return nil
	})
	if err != nil {
		return errors.Wrap(err, "timed out attempting to join member list")
	}

	// Add the local node to the event handler's peer list
	m.events.addPeer(node)

	return nil
}

func (m *MemberListPool) Close() {
	err := m.memberList.Leave(clock.Second)
	if err != nil {
		m.log.Warn(errors.Wrap(err, "while leaving member-list"))
	}
}

type memberListEventHandler struct {
	peers map[string]PeerInfo
	log   FieldLogger
	conf  MemberListPoolConfig
}

func newMemberListEventHandler(log FieldLogger, conf MemberListPoolConfig) *memberListEventHandler {
	handler := memberListEventHandler{
		conf: conf,
		log:  log,
	}
	handler.peers = make(map[string]PeerInfo)
	return &handler
}

func (e *memberListEventHandler) addPeer(node *ml.Node) {
	ip := getIP(node.Address())

	peer, err := unmarshallPeer(node.Meta, ip)
	if err != nil {
		e.log.WithError(err).Warnf("while adding to peers")
	} else {
		e.peers[ip] = peer
		e.callOnUpdate()
	}
}

func (e *memberListEventHandler) NotifyJoin(node *ml.Node) {
	ip := getIP(node.Address())

	peer, err := unmarshallPeer(node.Meta, ip)
	if err != nil {
		// This is called during member list initialization due to the fact that the local node
		// has no metadata yet
		e.log.WithError(err).Warn("while deserialize member-list peer")
		return
	}
	peer.IsOwner = false
	e.peers[ip] = peer
	e.callOnUpdate()
}

func (e *memberListEventHandler) NotifyLeave(node *ml.Node) {
	ip := getIP(node.Address())

	// Remove PeerInfo
	delete(e.peers, ip)

	e.callOnUpdate()
}

func (e *memberListEventHandler) NotifyUpdate(node *ml.Node) {
	ip := getIP(node.Address())

	peer, err := unmarshallPeer(node.Meta, ip)
	if err != nil {
		e.log.WithError(err).Warn("while unmarshalling peer info")
	}
	peer.IsOwner = false
	e.peers[ip] = peer
	e.callOnUpdate()
}

func (e *memberListEventHandler) callOnUpdate() {
	var peers []PeerInfo

	for _, p := range e.peers {
		if p.GRPCAddress == e.conf.Advertise.GRPCAddress {
			p.IsOwner = true
		}
		peers = append(peers, p)
	}
	e.conf.OnUpdate(peers)
}

func getIP(address string) string {
	addr, _, _ := net.SplitHostPort(address)
	return addr
}

func makeAddress(ip string, port int) string {
	return net.JoinHostPort(ip, strconv.Itoa(port))
}

// Deprecated
type memberListMetadata struct {
	DataCenter       string
	AdvertiseAddress string
	GubernatorPort   int
}

func unmarshallPeer(b []byte, ip string) (PeerInfo, error) {
	var peer PeerInfo
	if err := json.Unmarshal(b, &peer); err != nil {
		var metadata memberListMetadata
		decoder := gob.NewDecoder(bytes.NewBuffer(b))
		if err := decoder.Decode(&peer); err != nil {
			return peer, errors.Wrap(err, "error decoding peer")
		}
		// Handle deprecated GubernatorPort
		if metadata.AdvertiseAddress == "" {
			metadata.AdvertiseAddress = makeAddress(ip, metadata.GubernatorPort)
		}
		return PeerInfo{GRPCAddress: metadata.AdvertiseAddress, DataCenter: metadata.DataCenter}, nil
	}
	return peer, nil
}

func newLogWriter(log FieldLogger) *io.PipeWriter {
	reader, writer := io.Pipe()

	go func() {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			log.Info(scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			log.Errorf("Error while reading from Writer: %s", err)
		}
		reader.Close()
	}()
	runtime.SetFinalizer(writer, func(w *io.PipeWriter) {
		writer.Close()
	})

	return writer
}

func splitAddress(addr string) (string, int, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return host, 0, errors.New(" expected format is `address:port`")
	}

	intPort, err := strconv.Atoi(port)
	if err != nil {
		return host, intPort, errors.Wrap(err, "port must be a number")
	}
	return host, intPort, nil
}
