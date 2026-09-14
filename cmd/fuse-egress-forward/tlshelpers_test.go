package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"testing"
	"time"
)

// testCredential is a CA plus one leaf, in the PEM shapes the forwarder's flags
// take. It stands in for what the proxy's Enroll mints and what the Secret
// mounts, WITHOUT importing internal/tools/sandbox: this binary is deliberately
// dependency-free (a static CGO_ENABLED=0 build with nothing of fuse's own in
// it), so its tests mint their own material rather than reaching for the proxy's.
type testCredential struct {
	certPEM []byte
	keyPEM  []byte
	caPEM   []byte

	// serverCert is the same CA's SERVER leaf, so an echo server can present
	// something the client above will verify.
	serverCert tls.Certificate
	// clientPool verifies the client leaf, so the echo server can require mTLS
	// exactly as the real proxy does.
	clientPool *x509.CertPool
}

// newTestCredential mints an independent CA, a server leaf for
// "fuse-egress-proxy" and 127.0.0.1, and one client leaf.
func newTestCredential(t *testing.T) testCredential {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test egress CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("CreateCertificate (CA): %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("ParseCertificate (CA): %v", err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	leaf := func(cn string, server bool) (tls.Certificate, []byte, []byte) {
		t.Helper()
		key, kerr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if kerr != nil {
			t.Fatalf("GenerateKey: %v", kerr)
		}
		usage := x509.ExtKeyUsageClientAuth
		if server {
			usage = x509.ExtKeyUsageServerAuth
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    time.Now().Add(-time.Minute),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		}
		if server {
			tmpl.DNSNames = []string{proxyServerName}
			tmpl.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
		}
		der, cerr := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		if cerr != nil {
			t.Fatalf("CreateCertificate: %v", cerr)
		}
		keyDER, merr := x509.MarshalPKCS8PrivateKey(key)
		if merr != nil {
			t.Fatalf("MarshalPKCS8PrivateKey: %v", merr)
		}
		parsed, perr := x509.ParseCertificate(der)
		if perr != nil {
			t.Fatalf("ParseCertificate: %v", perr)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: parsed},
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	}

	srvCert, _, _ := leaf(proxyServerName, true)
	_, clientCertPEM, clientKeyPEM := leaf("test client", false)

	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	return testCredential{
		certPEM:    clientCertPEM,
		keyPEM:     clientKeyPEM,
		caPEM:      caPEM,
		serverCert: srvCert,
		clientPool: pool,
	}
}

// tlsEchoServer stands in for fuse's own TLS listener: mutual TLS, then the same
// line-prefixing echo the UNIX-socket helper does, so a test can prove the bytes
// crossed the transport unchanged.
func tlsEchoServer(t *testing.T) (string, testCredential) {
	t.Helper()
	cred := newTestCredential(t)

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cred.serverCert},
		ClientCAs:    cred.clientPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	})
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				br := bufio.NewReader(conn)
				for {
					line, rerr := br.ReadString('\n')
					if line != "" {
						if _, werr := io.WriteString(conn, "upstream:"+line); werr != nil {
							return
						}
					}
					if rerr != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String(), cred
}
