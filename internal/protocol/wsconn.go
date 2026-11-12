package protocol

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WSConn adapts a *websocket.Conn into a net.Conn (and io.ReadWriteCloser)
// suitable for yamux.Server and yamux.Client using binary frames.
type WSConn struct {
	conn   *websocket.Conn
	reader io.Reader
	readMu sync.Mutex
	writeMu sync.Mutex
}

func NewWSConn(conn *websocket.Conn) *WSConn {
	return &WSConn{conn: conn}
}

func (c *WSConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	for {
		if c.reader == nil {
			msgType, r, err := c.conn.NextReader()
			if err != nil {
				return 0, err
			}
			if msgType != websocket.BinaryMessage && msgType != websocket.TextMessage {
				continue
			}
			c.reader = r
		}
		n, err := c.reader.Read(p)
		if err == io.EOF {
			c.reader = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (c *WSConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	err := c.conn.WriteMessage(websocket.BinaryMessage, p)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *WSConn) Close() error {
	c.writeMu.Lock()
	_ = c.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(2*time.Second),
	)
	c.writeMu.Unlock()
	return c.conn.Close()
}

func (c *WSConn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

func (c *WSConn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

func (c *WSConn) SetDeadline(t time.Time) error {
	if err := c.conn.SetReadDeadline(t); err != nil {
		return err
	}
	return c.conn.SetWriteDeadline(t)
}

func (c *WSConn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

func (c *WSConn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}
