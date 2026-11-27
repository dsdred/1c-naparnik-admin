package app

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"naparnikadmin/internal/config"
)

func TestReloadTLSCertificateAtSamePaths(t *testing.T) {
	a, _ := newConfigTestApp(t)
	dir := t.TempDir()
	spec := config.Listener{
		Kind: config.KindGateway, Addr: "127.0.0.1:0",
		CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key"),
	}
	roots := x509.NewCertPool()
	writePair := func(serial int64) {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		cert := &x509.Certificate{
			SerialNumber: big.NewInt(serial), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			KeyUsage: x509.KeyUsageDigitalSignature,
		}
		der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		pair, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		roots.AppendCertsFromPEM(certPEM)
		if err := os.WriteFile(spec.CertFile, certPEM, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(spec.KeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pair}), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writePair(1)
	if err := a.ApplyListeners([]config.Listener{spec}); err != nil {
		t.Fatal(err)
	}
	socket := a.servers[listenerKey(spec)].ln
	assertSerial := func(want int64) {
		t.Helper()
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", socket.Addr().String(), &tls.Config{
			RootCAs: roots, MinVersion: tls.VersionTLS12,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if got := conn.ConnectionState().PeerCertificates[0].SerialNumber.Int64(); got != want {
			t.Fatalf("served certificate serial %d, want %d", got, want)
		}
	}
	assertSerial(1)
	writePair(2)
	saveErr := errors.New("save failed")
	if err := a.applyListeners([]config.Listener{spec}, func() error { return saveErr }); !errors.Is(err, saveErr) {
		t.Fatalf("expected save failure, got %v", err)
	}
	assertSerial(1)
	if err := a.ApplyListeners([]config.Listener{spec}); err != nil {
		t.Fatal(err)
	}
	if a.servers[listenerKey(spec)].ln != socket {
		t.Fatal("TLS reload replaced the socket")
	}
	assertSerial(2)
	if err := os.WriteFile(spec.KeyFile, []byte("invalid PEM"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyListeners([]config.Listener{spec}); err == nil {
		t.Fatal("accepted an invalid key")
	}
	assertSerial(2)
}
