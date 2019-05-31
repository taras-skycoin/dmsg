package dms

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"sync"
	"time"

	"github.com/prometheus/common/log"
	"github.com/skycoin/skycoin/src/util/logging"

	"github.com/skycoin/skywire/internal/noise"
	"github.com/skycoin/skywire/pkg/cipher"
	"github.com/skycoin/skywire/pkg/messaging-discovery/client"
	"github.com/skycoin/skywire/pkg/transport"
)

var (
	// ErrNoSrv indicate that remote client does not have DelegatedServers in entry.
	ErrNoSrv = errors.New("remote has no DelegatedServers")
	// ErrClientClosed indicates that client is closed and not accepting new connections.
	ErrClientClosed = errors.New("client closed")
)

// Conn represents a connection between a dms.Client and dms.Server from a client's perspective.
type Conn struct {
	log *logging.Logger

	net.Conn                // conn to dms server
	local     cipher.PubKey // local client's pk
	remoteSrv cipher.PubKey // dms server's public key

	// nextID keeps track of unused tp_ids to assign a future locally-initiated tp.
	// locally-initiated tps use an even tp_id between local and intermediary dms_server.
	nextID uint16

	// map of transports to remote dms_clients (key: tp_id, val: transport).
	tps [math.MaxUint16]*Transport
	mx  sync.RWMutex // to protect tps.

	// awaits .Serve() to end before considering properly closed.
	wg sync.WaitGroup
}

// NewConn creates a new Conn.
func NewConn(log *logging.Logger, conn net.Conn, local, remote cipher.PubKey) *Conn {
	return &Conn{log: log, Conn: conn, local: local, remoteSrv: remote, nextID: 0}
}

func (c *Conn) delTp(id uint16) {
	c.mx.Lock()
	c.tps[id] = nil
	c.mx.Unlock()
}

func (c *Conn) setTp(tp *Transport) {
	c.mx.Lock()
	c.tps[tp.id] = tp
	c.mx.Unlock()
}

// keeps record of a locally-initiated tp to 'clientPK'.
// assigns an even tp_id and keeps track of it in tps map.
func (c *Conn) addTp(ctx context.Context, clientPK cipher.PubKey) (*Transport, error) {
	c.mx.Lock()
	defer c.mx.Unlock()

	for {
		if ch := c.tps[c.nextID]; ch == nil || ch.IsDone() {
			break
		}
		c.nextID += 2

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
	}

	id := c.nextID
	c.nextID = id + 2
	ch := NewTransport(c.Conn, c.local, clientPK, id)
	c.tps[id] = ch
	return ch, nil
}

func (c *Conn) getTp(id uint16) (*Transport, bool) {
	c.mx.RLock()
	tp := c.tps[id]
	c.mx.RUnlock()
	ok := tp != nil && !tp.IsDone()
	return tp, ok
}

func (c *Conn) handleRequestFrame(ctx context.Context, id uint16, p []byte) (*Transport, error) {
	// remote-initiated tps should:
	// - have a payload structured as 'init_pk:resp_pk'.
	// - resp_pk should be of local client.
	// - use an odd tp_id with the intermediary dms_server.
	initPK, respPK, ok := splitPKs(p)
	if !ok || respPK != c.local || isEven(id) {
		if err := writeCloseFrame(c.Conn, id, 0); err != nil {
			return nil, err
		}
		return nil, ErrRequestCheckFailed
	}

	tp := NewTransport(c.Conn, c.local, initPK, id)
	if err := tp.Handshake(ctx); err != nil {
		// return err here as response handshake is send via Conn and that shouldn't fail.
		return nil, err
	}
	c.setTp(tp)
	return tp, nil
}

// Serve handles incoming frames.
// Remote-initiated tps that are successfully created are pushing into 'accept' and exposed via 'Client.Accept()'.
func (c *Conn) Serve(ctx context.Context, accept chan<- *Transport) error {
	c.wg.Add(1)
	defer c.wg.Done()

	log := c.log.WithField("remoteSrv", c.remoteSrv)

	for {
		f, err := readFrame(c.Conn)
		if err != nil {
			return err
		}
		ft, id, p := f.Disassemble()
		tp, ok := c.getTp(id)
		log.Infof("readFrame: frameType(%v) channelID(%v) payloadLen(%v)", ft, id, f.PayLen())

		// if tp does not exist, frame should be 'REQUEST'.
		// otherwise, handle any unexpected frames accordingly.
		if !ok {
			c.delTp(id) // rm tp in case closed tp is not fully removed.
			switch ft {
			case RequestType:
				tp, err := c.handleRequestFrame(ctx, id, p)
				if err != nil {
					log.WithError(err).Infof("transportRejected: remoteClient(%v) channelID(%v)", tp.remoteClient, tp.id)
					if err == ErrRequestCheckFailed {
						continue
					}
					return err
				}
				log.Infof("transportAccepted: remoteClient(%v) channelID(%v)", tp.remoteClient, tp.id)
				select {
				case accept <- tp:
				case <-ctx.Done():
					return ctx.Err()
				}
			case CloseType:
				log.Infof("closeFrameIgnored: transport untracked locally.")
			default:
				log.Infof("unexpectedFrameReceived: transport untracked locally.")
				if err := writeCloseFrame(c.Conn, id, 0); err != nil {
					return err
				}
			}
			continue
		}

		// If tp of tp_id exists, attempt to forward frame to tp.
		// delete tp on any failure.
		if !tp.AwaitRead(f) {
			c.delTp(id)
		}
	}
}

// DialTransport dials a transport to remote dms_client.
func (c *Conn) DialTransport(ctx context.Context, clientPK cipher.PubKey) (*Transport, error) {
	tp, err := c.addTp(ctx, clientPK)
	if err != nil {
		return nil, err
	}
	return tp, tp.Handshake(ctx)
}

// Close closes the connection to dms_server.
func (c *Conn) Close() error {
	c.log.Infof("closingLink: remoteSrv(%v)", c.remoteSrv)
	c.mx.Lock()
	for _, tp := range c.tps {
		if tp != nil {
			_ = tp.Close()
		}
	}
	err := c.Conn.Close()
	c.mx.Unlock()
	c.wg.Wait()
	return err
}

// Client implements transport.Factory
type Client struct {
	log *logging.Logger

	pk cipher.PubKey
	sk cipher.SecKey
	dc client.APIClient

	conns map[cipher.PubKey]*Conn // conns with messaging servers. Key: pk of server
	mx    sync.RWMutex

	accept chan *Transport
	once   sync.Once
}

// NewClient creates a new Client.
func NewClient(pk cipher.PubKey, sk cipher.SecKey, dc client.APIClient) *Client {
	return &Client{
		log:    logging.MustGetLogger("dms_client"),
		pk:     pk,
		sk:     sk,
		dc:     dc,
		conns:  make(map[cipher.PubKey]*Conn),
		accept: make(chan *Transport),
	}
}

// SetLogger sets the dms_client's logger.
func (c *Client) SetLogger(log *logging.Logger) {
	c.log = log
}

// TODO: re-connect logic.
//func (c *Client) setConn(l *Conn) {
//	c.mx.Lock()
//	c.conns[l.remoteSrv] = l
//	c.mx.Unlock()
//}

func (c *Client) delConn(pk cipher.PubKey) {
	c.mx.Lock()
	delete(c.conns, pk)
	c.mx.Unlock()
}

// TODO: re-connect logic.
//func (c *Client) getConn(pk cipher.PubKey) (*Conn, bool) {
//	c.mx.RLock()
//	l, ok := c.conns[pk]
//	c.mx.RUnlock()
//	return l, ok
//}

func (c *Client) newConn(ctx context.Context, srvPK cipher.PubKey, addr string) (*Conn, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	ns, err := noise.New(noise.HandshakeXK, noise.Config{
		LocalPK:   c.pk,
		LocalSK:   c.sk,
		RemotePK:  srvPK,
		Initiator: true,
	})
	if err != nil {
		return nil, err
	}
	nc, err := noise.WrapConn(conn, ns, hsTimeout)
	if err != nil {
		return nil, err
	}
	l := NewConn(c.log, nc, c.pk, srvPK)
	go func() {
		if err := l.Serve(ctx, c.accept); err != nil {
			l.log.WithError(err).WithField("srv_pk", l.remoteSrv).Warn("link with server closed")
			if err := c.updateDiscEntry(ctx); err != nil {
				c.log.WithError(err).Error("failed to update entry after server close.")
			}
			c.delConn(l.remoteSrv)
		}
	}()
	return l, nil
}

// InitiateServers initiates connections with dms_servers.
func (c *Client) InitiateServers(ctx context.Context, n int) error {
	if n == 0 {
		return nil
	}
	var entries []*client.Entry
	var err error
	for {
		if entries, err = c.dc.AvailableServers(ctx); err != nil || len(entries) == 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("messaging servers are not available: %s", err)
			default:
				time.Sleep(time.Second)
				continue
			}
		}
		break
	}
	for _, entry := range entries {
		if len(c.conns) > n {
			break
		}
		conn, err := c.newConn(ctx, entry.Static, entry.Server.Address)
		if err != nil {
			log.Warnf("Failed to connect to server %s: %s", entry.Static, err)
			continue
		}
		c.conns[conn.remoteSrv] = conn
	}
	if len(c.conns) == 0 {
		return fmt.Errorf("servers are not available: all servers failed")
	}
	if err := c.updateDiscEntry(ctx); err != nil {
		return fmt.Errorf("updating client's discovery entry failed with: %s", err)
	}
	return nil
}

func (c *Client) findConn(ctx context.Context, srvPKs []cipher.PubKey) (*Conn, error) {
	for _, srvPK := range srvPKs {
		conn, ok := c.conns[srvPK]
		if !ok {
			continue
		}
		return conn, nil
	}
	for _, srvPK := range srvPKs {
		entry, err := c.dc.Entry(ctx, srvPK)
		if err != nil {
			return nil, fmt.Errorf("get server failure: %s", err)
		}
		conn, err := c.newConn(ctx, entry.Static, entry.Server.Address)
		if err != nil {
			log.Warnf("Failed to connect to server %s: %s", entry.Static, err)
			continue
		}
		c.conns[conn.remoteSrv] = conn
		return conn, nil
	}
	return nil, ErrNoSrv
}

func (c *Client) updateDiscEntry(ctx context.Context) error {
	log.Info("updatingEntry")
	var srvPKs []cipher.PubKey
	c.mx.RLock()
	for pk := range c.conns {
		srvPKs = append(srvPKs, pk)
	}
	c.mx.RUnlock()
	entry, err := c.dc.Entry(ctx, c.pk)
	if err != nil {
		entry = client.NewClientEntry(c.pk, 0, srvPKs)
		if err := entry.Sign(c.sk); err != nil {
			return err
		}
		return c.dc.SetEntry(ctx, entry)
	}
	entry.Client.DelegatedServers = srvPKs
	return c.dc.UpdateEntry(ctx, c.sk, entry)
}

// Accept accepts remotely-initiated tps.
func (c *Client) Accept(ctx context.Context) (transport.Transport, error) {
	select {
	case tp, ok := <-c.accept:
		if !ok {
			return nil, ErrClientClosed
		}
		return tp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Dial dials a transport to remote dms_client.
func (c *Client) Dial(ctx context.Context, remote cipher.PubKey) (transport.Transport, error) {
	c.mx.Lock()
	defer c.mx.Unlock()

	entry, err := c.dc.Entry(ctx, remote)
	if err != nil {
		return nil, fmt.Errorf("get entry failure: %s", err)
	}
	if len(entry.Client.DelegatedServers) == 0 {
		return nil, ErrNoSrv
	}
	conn, err := c.findConn(ctx, entry.Client.DelegatedServers)
	if err != nil {
		return nil, err
	}
	return conn.DialTransport(ctx, remote)
}

// Local returns the local dms_client's public key.
func (c *Client) Local() cipher.PubKey {
	return c.pk
}

// Type returns the transport type.
func (c *Client) Type() string {
	return Type
}

// Close closes the dms_client and associated connections.
// TODO(evaninjin): proper error handling.
func (c *Client) Close() error {
	c.mx.Lock()
	defer c.mx.Unlock()

	for _, link := range c.conns {
		_ = link.Close()
	}
	c.conns = make(map[cipher.PubKey]*Conn)
	c.once.Do(func() {
		close(c.accept)
	})
	return nil
}
