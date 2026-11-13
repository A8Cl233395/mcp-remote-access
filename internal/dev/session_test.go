package dev

import (
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"mcp-remoteaccess/internal/protocol"
)

func TestLocalToRemoteRemoveDuringReconnectNoStreamLeak(t *testing.T) {
	cConn, sConn := net.Pipe()
	defer cConn.Close()
	defer sConn.Close()

	devYamux, err := yamux.Client(cConn, protocol.DefaultYamuxConfig())
	if err != nil {
		t.Fatalf("yamux client: %v", err)
	}
	defer devYamux.Close()

	agentYamux, err := yamux.Server(sConn, protocol.DefaultYamuxConfig())
	if err != nil {
		t.Fatalf("yamux server: %v", err)
	}
	defer agentYamux.Close()

	secondStreamOpened := make(chan struct{}, 1)
	secondStreamClosed := make(chan struct{}, 1)

	go func() {
		// 1st reverse_listen stream: accept, ack, then close immediately to trigger reconnect loop
		st1, err := agentYamux.AcceptStream()
		if err != nil {
			return
		}
		bs1 := protocol.NewBufferedStream(st1)
		var h1 protocol.StreamHeader
		if err := bs1.ReadLineJSON(&h1); err != nil {
			return
		}
		_ = protocol.WriteLineJSON(bs1, protocol.StreamAck{OK: true, BoundAddr: "127.0.0.1:41001"})
		time.Sleep(20 * time.Millisecond)
		_ = st1.Close()

		// 2nd reverse_listen stream (during reconnect): wait a bit before ACKing so removeForward fires mid-handshake
		st2, err := agentYamux.AcceptStream()
		if err != nil {
			return
		}
		defer st2.Close()
		bs2 := protocol.NewBufferedStream(st2)
		var h2 protocol.StreamHeader
		if err := bs2.ReadLineJSON(&h2); err != nil {
			return
		}
		close(secondStreamOpened)

		time.Sleep(300 * time.Millisecond)
		_ = protocol.WriteLineJSON(bs2, protocol.StreamAck{OK: true, BoundAddr: "127.0.0.1:41002"})

		// Wait to see if dev-client closes the leaked stream
		buf := make([]byte, 16)
		_, _ = bs2.Read(buf)
		close(secondStreamClosed)
	}()

	sess := NewSession(SessionConfig{DefaultMachine: "m1"})
	sess.yamuxSess = devYamux
	defer sess.Close()

	_, meta, err := sess.Forwarding("add", "", "tcp", "local_to_remote", "127.0.0.1:9999", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Forwarding add failed: %v", err)
	}
	id := meta["id"].(string)

	// Wait until the reconnect loop actually opens the 2nd reverse_listen stream
	select {
	case <-secondStreamOpened:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for reconnect reverse_listen stream")
	}

	// Remove the rule while openReverseListen is in flight waiting for ACK
	if _, _, err := sess.Forwarding("remove", id, "", "", "", ""); err != nil {
		t.Fatalf("Forwarding remove failed: %v", err)
	}

	// If the newly opened stream is not closed after removeForward, it leaks and blocks forever
	select {
	case <-secondStreamClosed:
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("reconnect reverse_listen stream leaked and stayed open after removeForward")
	}
}
