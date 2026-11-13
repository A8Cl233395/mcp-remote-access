package relay

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// BuildTLSConfig loads a trusted X.509 certificate and private key pair from disk,
// or if insecure is true and no cert files are given, generates an ephemeral self-signed certificate.
func BuildTLSConfig(certFile, keyFile string, insecure bool) (*tls.Config, error) {
	if certFile != "" || keyFile != "" {
		if certFile == "" || keyFile == "" {
			return nil, fmt.Errorf("both -tls-cert and -tls-key must be provided")
		}
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("load tls keypair: %w", err)
		}
		return &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		}, nil
	}

	if !insecure {
		return nil, fmt.Errorf("SSL certificate is required by default (-tls-cert and -tls-key); pass -insecure to run without a formal SSL certificate")
	}

	cert, err := generateSelfSignedCert()
	if err != nil {
		return nil, fmt.Errorf("generate self-signed cert: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}, nil
}

func generateSelfSignedCert() (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, err
	}

	host, _ := os.Hostname()
	if host == "" {
		host = "mcp-relay-server"
	}

	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   host,
			Organization: []string{"mcp-remoteaccess"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost", host},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	privBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privBytes})

	return tls.X509KeyPair(certPEM, keyPEM)
}

type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}

// flexConn defers first-byte TLS-vs-plain detection from Accept() into the connection's
// own goroutine so an idle TCP client never stalls the listener's Accept loop.
type flexConn struct {
	raw       net.Conn
	tlsConfig *tls.Config

	initOnce sync.Once
	ready    atomic.Bool
	active   net.Conn

	dlMu              sync.Mutex
	hasReadDeadline   bool
	savedReadDeadline time.Time
}

func (c *flexConn) detect() {
	br := bufio.NewReaderSize(c.raw, 4096)

	c.dlMu.Lock()
	hasDL := c.hasReadDeadline
	savedDL := c.savedReadDeadline
	c.dlMu.Unlock()

	if !hasDL {
		_ = c.raw.SetReadDeadline(time.Now().Add(10 * time.Second))
	}
	first, err := br.Peek(1)
	if !hasDL {
		_ = c.raw.SetReadDeadline(time.Time{})
	} else {
		_ = c.raw.SetReadDeadline(savedDL)
	}

	pConn := &peekedConn{Conn: c.raw, r: br}
	if err == nil && len(first) == 1 && first[0] == 0x16 && c.tlsConfig != nil {
		c.active = tls.Server(pConn, c.tlsConfig)
	} else {
		c.active = pConn
	}
	c.ready.Store(true)
}

func (c *flexConn) Read(p []byte) (int, error) {
	c.initOnce.Do(c.detect)
	return c.active.Read(p)
}

func (c *flexConn) Write(p []byte) (int, error) {
	c.initOnce.Do(c.detect)
	return c.active.Write(p)
}

func (c *flexConn) Close() error {
	if c.ready.Load() {
		return c.active.Close()
	}
	return c.raw.Close()
}

func (c *flexConn) LocalAddr() net.Addr {
	return c.raw.LocalAddr()
}

func (c *flexConn) RemoteAddr() net.Addr {
	return c.raw.RemoteAddr()
}

func (c *flexConn) SetDeadline(t time.Time) error {
	if c.ready.Load() {
		return c.active.SetDeadline(t)
	}
	c.dlMu.Lock()
	c.hasReadDeadline = !t.IsZero()
	c.savedReadDeadline = t
	c.dlMu.Unlock()
	return c.raw.SetDeadline(t)
}

func (c *flexConn) SetReadDeadline(t time.Time) error {
	if c.ready.Load() {
		return c.active.SetReadDeadline(t)
	}
	c.dlMu.Lock()
	c.hasReadDeadline = !t.IsZero()
	c.savedReadDeadline = t
	c.dlMu.Unlock()
	return c.raw.SetReadDeadline(t)
}

func (c *flexConn) SetWriteDeadline(t time.Time) error {
	if c.ready.Load() {
		return c.active.SetWriteDeadline(t)
	}
	return c.raw.SetWriteDeadline(t)
}

// FlexTLSListener wraps a net.Listener. If allowPlainHTTP is true (in -insecure mode),
// it wraps accepted connections so the first incoming byte is peeked per-connection:
// if it is 0x16 (TLS Handshake), it negotiates TLS; otherwise it passes through as plain HTTP/WS.
type FlexTLSListener struct {
	net.Listener
	tlsConfig      *tls.Config
	allowPlainHTTP bool
}

func NewFlexTLSListener(inner net.Listener, tlsCfg *tls.Config, allowPlainHTTP bool) net.Listener {
	if !allowPlainHTTP {
		return tls.NewListener(inner, tlsCfg)
	}
	return &FlexTLSListener{
		Listener:       inner,
		tlsConfig:      tlsCfg,
		allowPlainHTTP: true,
	}
}

func (l *FlexTLSListener) Accept() (net.Conn, error) {
	rawConn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &flexConn{
		raw:       rawConn,
		tlsConfig: l.tlsConfig,
	}, nil
}
