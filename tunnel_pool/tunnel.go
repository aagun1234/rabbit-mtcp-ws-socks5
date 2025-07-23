package tunnel_pool

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/aagun1234/rabbit-mtcp-ws/block"
	"github.com/aagun1234/rabbit-mtcp-ws/logger"
	"github.com/aagun1234/rabbit-mtcp-ws/tunnel"
	"io"
	"math/rand"
	"net"
	"time"
	"github.com/gorilla/websocket"
	"go.uber.org/atomic"
)

type Tunnel struct {
	net.Conn
	//websocket.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	tunnelID uint32
	peerID   uint32
	logger   *logger.Logger
	RecvBytes  uint64
	SentBytes  uint64
	LastActivity  atomic.Int64
	LatencyNano   atomic.Int64
}



// Create a new tunnel from a net.Conn and cipher with random tunnelID
//func NewActiveTunnel(conn net.Conn, ciph tunnel.Cipher, peerID uint32) (Tunnel, error) {
func NewActiveTunnel(wsConn *websocket.Conn, ciph tunnel.Cipher, peerID uint32) (Tunnel, error) {
	tun := newTunnelWithID(wsConn, ciph, peerID)
	return tun, tun.activeExchangePeerID()
}

//func NewPassiveTunnel(conn net.Conn, ciph tunnel.Cipher) (Tunnel, error) {
func NewPassiveTunnel(wsConn *websocket.Conn, ciph tunnel.Cipher) (Tunnel, error) {
	tun := newTunnelWithID(wsConn, ciph, 0)
	return tun, tun.passiveExchangePeerID()
}

// Create a new tunnel from a net.Conn and cipher with given tunnelID
//func newTunnelWithID(conn net.Conn, ciph tunnel.Cipher, peerID uint32) Tunnel {
func newTunnelWithID(wsConn *websocket.Conn, ciph tunnel.Cipher, peerID uint32) Tunnel {
	tunnelID := rand.Uint32()
	tun := Tunnel{
		//Conn:     tunnel.NewEncryptedConn(conn, ciph),
		Conn:     &WebsocketConnAdapter{Conn: wsConn},
		peerID:   peerID,
		tunnelID: tunnelID,
		logger:   logger.NewLogger(fmt.Sprintf("[Tunnel-%d]", tunnelID)),
	}
	tun.logger.InfoAf("Tunnel %d created.", tunnelID)
	return tun
}




func (tunnel *Tunnel) SetLastActive() {
	tunnel.LastActivity.Store(time.Now().UnixNano())
}

func (tunnel *Tunnel) GetLastActiveStr() string {
	return time.Unix(0, tunnel.LastActivity.Load()).Format("2006-01-02 15:04:05.999999")
}

func (tunnel *Tunnel) GetLastActive() int64 {
	return tunnel.LastActivity.Load()
}


func (tunnel *Tunnel) SetLatencyNanoSince(timestamp int64) {	
	tunnel.LatencyNano.Store(time.Now().UnixNano()-timestamp)
}
func (tunnel *Tunnel) SetLatencyNano(latency int64) {	
	tunnel.LatencyNano.Store(latency)
}
func (tunnel *Tunnel) GetLatencyNano() int64 {	
	return tunnel.LatencyNano.Load()
}


func (tunnel *Tunnel) activeExchangePeerID() (err error) {
	err = tunnel.sendPeerID(tunnel.peerID)
	if err != nil {
		tunnel.logger.Errorf("Cannot exchange peerID(send failed: %v).\n", err)
		return err
	}
	peerID, err := tunnel.recvPeerID()
	if err != nil {
		tunnel.logger.Errorf("Cannot exchange peerID(recv failed: %v).\n", err)
		return err
	}
	if tunnel.peerID != peerID {
		tunnel.logger.Errorf("Cannot exchange peerID(local: %d, remote: %d).\n", tunnel.peerID, peerID)
		return errors.New("invalid exchanging")
	}
	tunnel.logger.Infof("PeerID %d exchange successfully.",peerID)
	return
}

func (tunnel *Tunnel) passiveExchangePeerID() (err error) {
	peerID, err := tunnel.recvPeerID()
	if err != nil {
		tunnel.logger.Errorf("Cannot exchange peerID(recv failed: %v).\n", err)
		return err
	}
	err = tunnel.sendPeerID(peerID)
	if err != nil {
		tunnel.logger.Errorf("Cannot exchange peerID(send failed: %v).\n", err)
		return err
	}
	tunnel.peerID = peerID
	tunnel.logger.Infof("PeerID %d exchange successfully.",peerID)
	return
}

func (tunnel *Tunnel) sendPeerID(peerID uint32) error {
	peerIDBuffer := make([]byte, 4)
	binary.LittleEndian.PutUint32(peerIDBuffer, peerID)
	_, err := io.CopyN(tunnel.Conn, bytes.NewReader(peerIDBuffer), 4)
	if err != nil {
		tunnel.logger.Errorf("Peer id sent with error:%v.\n", err)
		return err
	}
	tunnel.logger.Debugln("Peer id sent.")
	return nil
}

func (tunnel *Tunnel) recvPeerID() (uint32, error) {
	peerIDBuffer := make([]byte, 4)
	tunnel.logger.Debugf("recvPeerID Set tunnel.Conn ReadDeadline 30S.\n")
	deadline := time.Now().Add(30 * time.Second)
	tunnel.Conn.SetReadDeadline(deadline)
	_, err := io.ReadFull(tunnel.Conn, peerIDBuffer)
	tunnel.logger.Debugf("recvPeerID Set tunnel.Conn ReadDeadline unlimit.\n")
	tunnel.Conn.SetReadDeadline(time.Time{})
	if err != nil {
		tunnel.logger.Errorf("Peer id recv with error:%v.\n", err)
		return 0, err
	}
	peerID := binary.LittleEndian.Uint32(peerIDBuffer)
	tunnel.logger.Debugln("Peer id recv.")
	return peerID, nil
}

// Read block from send channel, pack it and send, client to server
func (tunnel *Tunnel) OutboundRelay(normalQueue, retryQueue chan block.Block) {
	tunnel.logger.InfoAf("Outbound relay started. (PeerID: %d)",tunnel.peerID)
	for {
		// cancel is of highest priority
		select {
		case <-tunnel.ctx.Done():
			tunnel.logger.InfoAf("Outbound relay(cancel) ended. (PeerID: %d)",tunnel.peerID)
			return
		default:
		}
		// retryQueue is of secondary highest priority
		select {
		case <-tunnel.ctx.Done():
			tunnel.logger.InfoAf("Outbound relay(retry) ended. (PeerID: %d)",tunnel.peerID)
			return
		case blk := <-retryQueue:
			tunnel.packThenSend(blk, retryQueue)
		default:
		}
		// normalQueue is of secondary highest priority
		select {
		case <-tunnel.ctx.Done():
			tunnel.logger.InfoAf("Outbound relay(normal) ended. (PeerID: %d)",tunnel.peerID)
			return
		case blk := <-retryQueue:
			tunnel.packThenSend(blk, retryQueue)
		case blk := <-normalQueue:
			tunnel.packThenSend(blk, retryQueue)
		}
	}
}

func (tunnel *Tunnel) packThenSend(blk block.Block, retryQueue chan block.Block) {
	dataToSend := blk.Pack()
	reader := bytes.NewReader(dataToSend)

	tunnel.Conn.SetWriteDeadline(time.Now().Add(time.Duration(TunnelBlockTimeoutSec) * time.Second))
	n, err := io.Copy(tunnel.Conn, reader)
 
	if (err != nil || n != int64(len(dataToSend))) && retryQueue!=nil {
		tunnel.logger.Warnf("Error when send bytes to tunnel: (n: %d, error: %v).\n", n, err)
		// Tunnel down and message has not been fully sent.
		tunnel.closeThenCancel()
		go func() {
			retryQueue <- blk
		}()
		// Use new goroutine to avoid channel blocked
	} else {
		tunnel.Conn.SetWriteDeadline(time.Time{})
		tunnel.logger.Debugf("Copied data to tunnel successfully(n: %d).\n", n)
	}
}

// Read bytes from connection, parse it to block then put in recv channel
func (tunnel *Tunnel) InboundRelay(output chan<- block.Block) {
	tunnel.logger.InfoAf("Inbound relay started. (PeerID: %d)",tunnel.peerID)
	for {
		select {
		case <-tunnel.ctx.Done():
			// Should read all before leave, or packet will be lost
			for {
				// Will never be blocked because the tunnel is closed
				blk, err := block.NewBlockFromReader(tunnel.Conn)
				if err == nil {
					tunnel.logger.Debugf("Block received from tunnel(type: %d) successfully after close.\n", blk.Type)

					output <- *blk
					
					
				} else {
					tunnel.logger.Debugf("Error when receiving block from tunnel after close: %v.\n", err)
					break
				}
			}
			tunnel.logger.InfoAf("Inbound relay ended. (PeerID: %d)",tunnel.peerID)
			return
		default:
			blk, err := block.NewBlockFromReader(tunnel.Conn)
			if err != nil {
				// Server will never close connection in normal cases
				tunnel.logger.Errorf("Error when receiving block from tunnel: %v.\n" , err)
				// Tunnel down and message has not been fully read.
				tunnel.closeThenCancel()
			} else {
				tunnel.logger.Debugf("Block received from tunnel(type: %d)successfully.\n" , blk.Type)
				tunnel.SetLastActive()
				if blk.Type == block.TypePing {
					clatency:=int64(binary.LittleEndian.Uint64(blk.BlockData))
					tunnel.SetLatencyNano(clatency)
					tunnel.logger.Infof("Ping-Pong client latency: %d us\n", clatency/1000)
						
					pongblk:= block.NewPongBlock(0,0,uint64(blk.TimeStamp))
					tunnel.logger.Debugf("Sending Pong to websocket, with payload timestamp: %s", time.Unix(0, blk.TimeStamp).Format("2006-01-02 15:04:05.999999"))
					tunnel.packThenSend(pongblk, nil)
						
				} else if blk.Type == block.TypePong {
					tunnel.logger.Debugf("InboundRelay received TypePong.\n")
					timestamp:=int64(binary.LittleEndian.Uint64(blk.BlockData))
					tunnel.SetLatencyNanoSince(timestamp)
					tunnel.logger.Infof("Ping-Pong Latency: %d us", tunnel.GetLatencyNano()/1000)
				} else {

					output <- *blk
				}
				
			}
		}
	}
}



// Read bytes from connection, parse it to block then put in recv channel
func (tunnel *Tunnel) PingPong() {
	tunnel.logger.InfoAln("PingPong started.")
	
	ticker := time.NewTicker(1*time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-tunnel.ctx.Done():
			tunnel.logger.InfoAln("PingPong ended.")
			return
		case <-ticker.C:

			//tunnel.logger.Debugf("%d,%d",(nowstamp-tunnel.LastActivity.Load())/1e9,PingInterval)
			if (time.Now().UnixNano()-tunnel.GetLastActive())/1e9 > int64(PingInterval) { 
				blk:= block.NewPingBlock(tunnel.tunnelID,0,uint64(tunnel.GetLatencyNano()))
				tunnel.logger.Debugf("Sending Ping to websocket, with local latency: %d us", tunnel.GetLatencyNano()/1000)
				tunnel.packThenSend(blk, nil)
			}
		}
	}
}


func (tunnel *Tunnel) GetPeerID() uint32 {
	return tunnel.peerID
}

func (tunnel *Tunnel) closeThenCancel() {
	tunnel.Close()
	tunnel.cancel()
}









//==========================================================
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