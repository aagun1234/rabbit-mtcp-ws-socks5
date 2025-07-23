package connection

import (
	"io"
	"net"
	"time"

	"github.com/aagun1234/rabbit-mtcp-ws-socks5/block"
	"github.com/aagun1234/rabbit-mtcp-ws-socks5/logger"
	"github.com/gorilla/websocket"
	"go.uber.org/atomic"
)

type HalfOpenConn interface {
	net.Conn
	CloseRead() error
	CloseWrite() error
}

type CloseWrite interface {
	CloseWrite() error
}

type CloseRead interface {
	CloseRead() error
}

type Connection interface {
	HalfOpenConn
	GetConnectionID() uint32
	getOrderedRecvQueue() chan block.Block
	getRecvQueue() chan block.Block

	RecvBlock(block.Block)

	SendConnect(address string)
	SendDisconnect(uint8)

	OrderedRelay(connection Connection) // Run orderedRelay infinitely
	Stop()                              // Stop all related relay and remove itself from connectionPool
}

type baseConnection struct {
	blockProcessor   blockProcessor
	connectionID     uint32
	closed           *atomic.Bool
	sendQueue        chan<- block.Block // Same as connectionPool
	recvQueue        chan block.Block
	orderedRecvQueue chan block.Block
	logger           *logger.Logger
	LastActivity     atomic.Int64
	LatencyNano      atomic.Int64
}

func (bc *baseConnection) SetLastActive() {
	bc.LastActivity.Store(time.Now().UnixNano())
}

func (bc *baseConnection) GetLastActiveStr() string {
	return time.Unix(0, bc.LastActivity.Load()).Format("2006-01-02 15:04:05.999999")
}

func (bc *baseConnection) GetLastActive() int64 {
	return bc.LastActivity.Load()
}

func (bc *baseConnection) SetLatencyNanoSince(timestamp int64) {
	bc.LatencyNano.Store(time.Now().UnixNano() - timestamp)
}
func (bc *baseConnection) GetLatencyNano() int64 {
	return bc.LatencyNano.Load()
}

func (bc *baseConnection) Stop() {
	bc.logger.Debugf("connection stop\n")
	bc.blockProcessor.removeFromPool()
}

func (bc *baseConnection) OrderedRelay(connection Connection) {
	bc.blockProcessor.OrderedRelay(connection)
}

func (bc *baseConnection) GetConnectionID() uint32 {
	return bc.connectionID
}

func (bc *baseConnection) getRecvQueue() chan block.Block {
	return bc.recvQueue
}

func (bc *baseConnection) getOrderedRecvQueue() chan block.Block {
	return bc.orderedRecvQueue
}

func (bc *baseConnection) RecvBlock(blk block.Block) {
	bc.recvQueue <- blk
}

func (bc *baseConnection) SendConnect(address string) {
	bc.logger.Debugf("Send connect to %s block.\n", address)
	blk := bc.blockProcessor.packConnect(address, bc.connectionID)
	bc.sendQueue <- blk
}

func (bc *baseConnection) SendDisconnect(shutdownType uint8) {
	bc.logger.Debugf("Send disconnect block: %v\n", shutdownType)
	blk := bc.blockProcessor.packDisconnect(bc.connectionID, shutdownType)
	bc.sendQueue <- blk
	if shutdownType == block.ShutdownBoth {
		bc.Stop()
	}
}

func (bc *baseConnection) sendData(data []byte) {
	bc.logger.Debugln("Send data block.")
	blocks := bc.blockProcessor.packData(data, bc.connectionID)
	for _, blk := range blocks {
		bc.sendQueue <- blk
	}
}

// ================================================
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
