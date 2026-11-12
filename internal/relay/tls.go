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

// FlexTLSListener wraps a net.Listener. If allowPlainHTTP is true (in -insecure mode),
// it peeks at the first byte of each incoming connection: if it is 0x16 (TLS Handshake),
// it negotiates TLS; otherwise it passes the connection through as plain HTTP/WS.
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
	br := bufio.NewReaderSize(rawConn, 4096)
	_ = rawConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	first, err := br.Peek(1)
	_ = rawConn.SetReadDeadline(time.Time{})
	pConn := &peekedConn{Conn: rawConn, r: br}
	if err == nil && len(first) == 1 && first[0] == 0x16 && l.tlsConfig != nil {
		return tls.Server(pConn, l.tlsConfig), nil
	}
	return pConn, nil
}
