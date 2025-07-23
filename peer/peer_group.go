package peer

import (
	"context"

	"github.com/aagun1234/rabbit-mtcp-ws-socks5/logger"
	"github.com/aagun1234/rabbit-mtcp-ws-socks5/tunnel"
	"github.com/aagun1234/rabbit-mtcp-ws-socks5/tunnel_pool"

	//"net"
	"io"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type PeerGroup struct {
	lock        sync.Mutex
	cipher      tunnel.Cipher
	peerMapping map[uint32]*ServerPeer
	logger      *logger.Logger
}

func NewPeerGroup(cipher tunnel.Cipher) PeerGroup {
	if initRand() != nil {
		panic("Error when initialize random seed.")
	}
	return PeerGroup{
		cipher:      cipher,
		peerMapping: make(map[uint32]*ServerPeer),
		logger:      logger.NewLogger("[PeerGroup]"),
	}
}

// Add a tunnel to it's peer; will create peer if not exists
func (pg *PeerGroup) AddTunnel(tunnel *tunnel_pool.Tunnel) error {
	// add tunnel to peer(if absent, create peer to peer_group)
	pg.lock.Lock()
	var peer *ServerPeer
	var ok bool

	peerID := tunnel.GetPeerID()
	if peer, ok = pg.peerMapping[peerID]; !ok {
		peerContext, removePeerFunc := context.WithCancel(context.Background())
		serverPeer := NewServerPeerWithID(peerID, peerContext, removePeerFunc)
		peer = &serverPeer
		pg.peerMapping[peerID] = peer
		pg.logger.InfoAf("Server Peer %d added to PeerGroup.\n", peerID)

		go func() {
			<-peerContext.Done()
			pg.RemovePeer(peerID)
		}()
	}
	pg.lock.Unlock()
	peer.tunnelPool.AddTunnel(tunnel)

	return nil
}

// Like AddTunnel, add a raw connection
func (pg *PeerGroup) AddTunnelFromConn(conn *websocket.Conn) error {
	tun, err := tunnel_pool.NewPassiveTunnel(conn, pg.cipher)
	if err != nil {
		conn.Close()
		return err
	}

	return pg.AddTunnel(&tun)
}

func (pg *PeerGroup) RemovePeer(peerID uint32) {
	pg.logger.InfoAf("Server Peer %d removed from peer group.\n", peerID)
	pg.lock.Lock()
	defer pg.lock.Unlock()
	delete(pg.peerMapping, peerID)
}

// ===================================
type WebsocketConnAdapter struct {
	*websocket.Conn
	reader io.Reader
}

func (c *WebsocketConnAdapter) Read(b []byte) (int, error) {
	// WebSocket消息可能是分帧的，需要处理消息边界
	if c.reader == nil {
		_, r, err := c.Conn.NextReader()
		if err != nil {
			return 0, err
		}
		c.reader = r
	}

	n, err := c.reader.Read(b)
	if err == io.EOF {
		c.reader = nil
		return n, nil
	}
	return n, err
}

func (c *WebsocketConnAdapter) Write(b []byte) (int, error) {
	err := c.Conn.WriteMessage(websocket.BinaryMessage, b)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

// 确保实现所有net.Conn接口方法
func (c *WebsocketConnAdapter) SetDeadline(t time.Time) error {
	err := c.SetReadDeadline(t)
	if err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}

func (c *WebsocketConnAdapter) SetReadDeadline(t time.Time) error {
	return c.Conn.SetReadDeadline(t)
}

func (c *WebsocketConnAdapter) SetWriteDeadline(t time.Time) error {
	return c.Conn.SetWriteDeadline(t)
}
