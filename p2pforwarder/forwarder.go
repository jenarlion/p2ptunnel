package p2pforwarder

import (
	"context"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/network"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/p2p/host/autorelay"
	libp2pquic "github.com/libp2p/go-libp2p/p2p/transport/quic"

	"github.com/libp2p/go-libp2p/core/routing"
	routing2 "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/libp2p/go-libp2p/p2p/security/noise"

	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	libp2ptls "github.com/libp2p/go-libp2p/p2p/security/tls"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	mh "github.com/multiformats/go-multihash"
	"github.com/sparkymat/appdir"
)

const (
	protocolTypeTCP byte = 0x00
	protocolTypeUDP byte = 0x01
)

// Forwarder - instance of P2P Forwarder
type Forwarder struct {
	host      host.Host
	openPorts *openPortsStore

	portsSubscriptions    map[peer.ID]chan *portsManifest
	portsSubscriptionsMux sync.Mutex

	portsSubscribers    map[peer.ID]struct{}
	portsSubscribersMux sync.Mutex
}

type openPortsStore struct {
	tcp *openPortsStoreMap
	udp *openPortsStoreMap
}

type openPortsStoreMap struct {
	ports map[uint16]context.Context
	mux   sync.Mutex
}

func newOpenPortsStore() *openPortsStore {
	return &openPortsStore{
		tcp: &openPortsStoreMap{
			ports: map[uint16]context.Context{},
		},
		udp: &openPortsStoreMap{
			ports: map[uint16]context.Context{},
		},
	}
}

// NewForwarder - instances Forwarder and connects it to libp2p network
func NewForwarder() (*Forwarder, context.CancelFunc, error) {
	priv, err := loadUserPrivKey()
	if err != nil {
		return nil, nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	h, err := createLibp2pHost(ctx, priv)
	if err != nil {
		cancel()
		return nil, nil, err
	}

	f := &Forwarder{
		host: h,

		openPorts: newOpenPortsStore(),

		portsSubscriptions: make(map[peer.ID]chan *portsManifest),
		portsSubscribers:   make(map[peer.ID]struct{}),
	}

	setDialHandler(f)
	setPortsSubHandler(f)

	return f, cancel, nil
}

func loadUserPrivKey() (priv crypto.PrivKey, err error) {
	krPath, err := appdir.AppInfo{
		Author: "nickname32",
		Name:   "P2P Forwarder",
	}.ConfigPath("keypair")
	if err != nil {
		return nil, err
	}

	pkFile, err := os.Open(krPath)

	if err == nil {
		defer pkFile.Close()

		b, err := ioutil.ReadAll(pkFile)
		if err != nil {
			return nil, err
		}

		priv, err = crypto.UnmarshalPrivateKey(b)
		if err != nil {
			return nil, err
		}

		return priv, nil
	}

	if !os.IsNotExist(err) {
		return nil, err
	}

	priv, _, err = crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		return nil, err
	}
	b, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		return nil, err
	}

	err = os.MkdirAll(filepath.Dir(krPath), os.ModePerm)
	if err != nil {
		return nil, err
	}
	newPkFile, err := os.Create(krPath)
	if err != nil {
		return nil, err
	}
	_, err = newPkFile.Write(b)
	if err != nil {
		return nil, err
	}
	err = newPkFile.Close()
	if err != nil {
		return nil, err
	}

	return priv, nil
}

const Protocol = "/p2ptunnel/0.1"

func createLibp2pHost(ctx context.Context, priv crypto.PrivKey) (host.Host, error) {
	var d *dht.IpfsDHT

	connmgr, _ := connmgr.NewConnManager(
		100, // Lowwater
		400, // HighWater,
		connmgr.WithGracePeriod(time.Minute),
	)
	var h, err = libp2p.New(
		libp2p.Identity(priv),

		libp2p.ListenAddrStrings(
			"/ip4/0.0.0.0/udp/0/quic",
			"/ip6/::/udp/0/quic",

			"/ip4/0.0.0.0/tcp/0",
			"/ip6/::/tcp/0",
		),

		libp2p.Transport(libp2pquic.NewTransport),
		libp2p.Transport(tcp.NewTCPTransport),
		//libp2p.Transport(websocket.New),

		libp2p.Security(noise.ID, noise.New),
		libp2p.Security(libp2ptls.ID, libp2ptls.New),

		libp2p.NATPortMap(),

		libp2p.EnableNATService(),
		libp2p.ConnectionManager(connmgr),

		libp2p.EnableRelay(),
		libp2p.EnableRelayService(),
		libp2p.EnableAutoRelay(autorelay.WithDefaultStaticRelays(), autorelay.WithCircuitV1Support(), autorelay.WithNumRelays(20)),
		libp2p.DefaultPeerstore,

		libp2p.Routing(func(h host.Host) (routing.PeerRouting, error) {
			var err error
			d, err = dht.New(ctx, h, dht.BootstrapPeers(dht.GetDefaultBootstrapPeerAddrInfos()...))
			return d, err
		}),
	)
	if err != nil {
		return nil, err
	}

	// This connects to public bootstrappers
	for _, addr := range dht.DefaultBootstrapPeers {
		pi, err := peer.AddrInfoFromP2pAddr(addr)
		if err != nil {
			panic(err)
		}
		h.Connect(ctx, *pi)
	}

	err = d.Bootstrap(ctx)
	if err != nil {
		return nil, err
	}

	d1 := routing2.NewRoutingDiscovery(d)

	go func() {
		_, err = d1.Advertise(ctx, Protocol)

		if err != nil {
			log.Println(err)
		}
	}()

	go func() {
		peerChan, err := d1.FindPeers(ctx, Protocol)
		if err != nil {
			panic(err)
		}

		for peer := range peerChan {
			if peer.ID == h.ID() {
				continue
			}
			if h.Network().Connectedness(peer.ID) != network.Connected {
				h.Connect(ctx, peer)
			}
		}
	}()

	return h, err
}

func nsToCid(ns string) (cid.Cid, error) {
	h, err := mh.Sum([]byte(ns), mh.SHA2_256, -1)
	if err != nil {
		return cid.Undef, err
	}

	return cid.NewCidV1(cid.Raw, h), nil
}

// ID returns id of Forwarder
func (f *Forwarder) ID() string {
	return f.host.ID().Pretty()
}

var onErrFn = func(err error) {
	println(err.Error())
}
var onInfoFn = func(str string) {
	println(str)
}

// OnError sets function which be called on error inside this package
func OnError(fn func(error)) {
	if fn == nil {
		return
	}
	onErrFn = fn
}

// OnInfo sets function which be called on information inside this package
func OnInfo(fn func(string)) {
	if fn == nil {
		return
	}
	onInfoFn = fn
}
