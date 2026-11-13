package protocol

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	CloseCodeKicked       = 4001
	CloseCodeRejected     = 4002
	CloseCodeUnauthorized = 4003
)

// WSConn adapts a *websocket.Conn into a net.Conn (and io.ReadWriteCloser)
// suitable for yamux.Server and yamux.Client using binary frames.
type WSConn struct {
	conn           *websocket.Conn
	reader         io.Reader
	readMu         sync.Mutex
	writeMu        sync.Mutex
	closeOnce      sync.Once
	closeErr       error
	remoteCloseMu  sync.Mutex
	remoteCloseErr *websocket.CloseError
}

func NewWSConn(conn *websocket.Conn) *WSConn {
	wc := &WSConn{conn: conn}
	defaultClose := conn.CloseHandler()
	conn.SetCloseHandler(func(code int, text string) error {
		wc.recordRemoteClose(&websocket.CloseError{Code: code, Text: text})
		if defaultClose != nil {
			return defaultClose(code, text)
		}
		return nil
	})
	return wc
}

func (c *WSConn) recordRemoteClose(ce *websocket.CloseError) {
	if ce == nil {
		return
	}
	c.remoteCloseMu.Lock()
	if c.remoteCloseErr == nil {
		c.remoteCloseErr = ce
	}
	c.remoteCloseMu.Unlock()
}

func (c *WSConn) RemoteCloseError() *websocket.CloseError {
	c.remoteCloseMu.Lock()
	defer c.remoteCloseMu.Unlock()
	return c.remoteCloseErr
}

func (c *WSConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	for {
		if c.reader == nil {
			msgType, r, err := c.conn.NextReader()
			if err != nil {
				var ce *websocket.CloseError
				if errors.As(err, &ce) {
					c.recordRemoteClose(ce)
				}
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
		if err != nil {
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				c.recordRemoteClose(ce)
			}
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

func (c *WSConn) CloseWithCode(code int, reason string) error {
	c.closeOnce.Do(func() {
		// RFC 6455 limits close frame payload to 125 bytes (2-byte code + 123-byte reason).
		if len(reason) > 120 {
			reason = reason[:120]
		}
		c.writeMu.Lock()
		_ = c.conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(code, reason),
			time.Now().Add(500*time.Millisecond),
		)
		c.writeMu.Unlock()
		c.closeErr = c.conn.Close()
	})
	return c.closeErr
}

func (c *WSConn) Close() error {
	return c.CloseWithCode(websocket.CloseNormalClosure, "")
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
