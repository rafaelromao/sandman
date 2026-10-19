package scaffold

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// tlsHosts serves HTTPS over in-memory connections: each host maps to the CA
// that issues its leaf certificate. It needs no listening socket.
type tlsHosts map[string]testCA

func newTLSHost(t *testing.T, hosts tlsHosts, ca testCA) string {
	t.Helper()
	name := fmt.Sprintf("host%d.test", len(hosts))
	hosts[name] = ca
	return name
}

func (h tlsHosts) leaf(t *testing.T, name string) tls.Certificate {
	t.Helper()
	ca := h[name]
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// dial connects to an in-memory HTTPS server for addr's host.
func (h tlsHosts) dial(t *testing.T) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		name, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if _, ok := h[name]; !ok {
			return nil, fmt.Errorf("unknown test host %s", name)
		}
		client, server, err := bufferedConnPair()
		if err != nil {
			return nil, err
		}
		cert := h.leaf(t, name)
		go func() {
			defer server.Close()
			conn := tls.Server(server, &tls.Config{Certificates: []tls.Certificate{cert}})
			if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
				return
			}
			_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		}()
		return client, nil
	}
}

// hostTrust is a TLSTrust whose host trust store holds trusted and whose
// public root set is public.
func hostTrust(t *testing.T, servers tlsHosts, hosts []string, trusted []testCA, public []testCA, publicAvailable bool) TLSTrust {
	pool := x509.NewCertPool()
	for _, ca := range trusted {
		pool.AddCert(ca.cert)
	}
	var publicCerts []*x509.Certificate
	for _, ca := range public {
		publicCerts = append(publicCerts, ca.cert)
	}
	return TLSTrust{
		Hosts:       hosts,
		Transport:   &http.Transport{DialContext: servers.dial(t), TLSClientConfig: &tls.Config{RootCAs: pool}},
		PublicRoots: func() ([]*x509.Certificate, bool) { return publicCerts, publicAvailable },
	}
}

func TestScaffold_CACertAutoCapturesInterceptionRoot(t *testing.T) {
	srv := tlsHosts{}
	proxy := newTestCA(t, "Corp TLS Inspection Root")
	public := newTestCA(t, "Public Root")
	hosts := []string{newTLSHost(t, srv, proxy), newTLSHost(t, srv, proxy)}
	dir := t.TempDir()
	var out bytes.Buffer

	s := &Scaffolder{TLSTrust: hostTrust(t, srv, hosts, []testCA{proxy, public}, []testCA{public}, true)}
	if err := s.Scaffold(dir, Options{BuildTools: "generic", CACerts: []string{CACertAuto}, Writer: &out}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("scaffold --ca-cert auto: %v", err)
	}

	files := scaffoldedCertFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("certs = %v, want captured proxy root", files)
	}
	data, _ := os.ReadFile(files[0])
	if string(data) != string(proxy.pem) {
		t.Fatalf("captured certificate is not the proxy root")
	}
	if !strings.Contains(out.String(), "Corp TLS Inspection Root") {
		t.Fatalf("output does not name captured root:\n%s", out.String())
	}
	if !strings.Contains(readScaffoldDockerfile(t, dir), caCertInstallLines) {
		t.Fatalf("Dockerfile does not trust captured root")
	}
}

func TestScaffold_CACertAutoWithPublicRootsChangesNothing(t *testing.T) {
	srv := tlsHosts{}
	public := newTestCA(t, "Public Root")
	hosts := []string{newTLSHost(t, srv, public)}
	dir := t.TempDir()
	var out bytes.Buffer

	s := &Scaffolder{TLSTrust: hostTrust(t, srv, hosts, []testCA{public}, []testCA{public}, true)}
	if err := s.Scaffold(dir, Options{BuildTools: "generic", CACerts: []string{CACertAuto}, Writer: &out}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("scaffold --ca-cert auto: %v", err)
	}
	if files := scaffoldedCertFiles(t, dir); len(files) != 0 {
		t.Fatalf("certs = %v, want none", files)
	}
	if !strings.Contains(out.String(), "no TLS interception detected") {
		t.Fatalf("output = %q, want no-interception report", out.String())
	}
}

func TestScaffold_CACertAutoKeepsExistingCertsWhenNothingDetected(t *testing.T) {
	srv := tlsHosts{}
	existing := newTestCA(t, "Existing Root")
	public := newTestCA(t, "Public Root")
	dir := t.TempDir()
	if err := (&Scaffolder{}).Scaffold(dir, Options{BuildTools: "generic", CACerts: []string{writeTestFile(t, existing.pem)}}, &fakePrompter{confirm: true}); err != nil {
		t.Fatal(err)
	}

	s := &Scaffolder{TLSTrust: hostTrust(t, srv, []string{newTLSHost(t, srv, public)}, []testCA{public}, []testCA{public}, true)}
	if err := s.Scaffold(dir, Options{BuildTools: "generic", CACerts: []string{CACertAuto}}, &fakePrompter{confirm: true}); err != nil {
		t.Fatal(err)
	}
	if files := scaffoldedCertFiles(t, dir); len(files) != 1 {
		t.Fatalf("certs = %v, want existing certificate kept", files)
	}
}

func TestScaffold_CACertAutoCombinesWithPath(t *testing.T) {
	srv := tlsHosts{}
	proxy := newTestCA(t, "Proxy Root")
	extra := newTestCA(t, "Extra Root")
	dir := t.TempDir()

	s := &Scaffolder{TLSTrust: hostTrust(t, srv, []string{newTLSHost(t, srv, proxy)}, []testCA{proxy}, nil, true)}
	if err := s.Scaffold(dir, Options{BuildTools: "generic", CACerts: []string{CACertAuto, writeTestFile(t, extra.pem)}}, &fakePrompter{confirm: true}); err != nil {
		t.Fatal(err)
	}
	if files := scaffoldedCertFiles(t, dir); len(files) != 2 {
		t.Fatalf("certs = %v, want proxy and extra roots", files)
	}
}

func TestScaffold_CACertAutoFallsBackToSharedRootWithoutPublicSet(t *testing.T) {
	srv := tlsHosts{}
	proxy := newTestCA(t, "Proxy Root")
	other := newTestCA(t, "Other Root")

	shared := t.TempDir()
	s := &Scaffolder{TLSTrust: hostTrust(t, srv, []string{newTLSHost(t, srv, proxy), newTLSHost(t, srv, proxy)}, []testCA{proxy}, nil, false)}
	if err := s.Scaffold(shared, Options{BuildTools: "generic", CACerts: []string{CACertAuto}}, &fakePrompter{confirm: true}); err != nil {
		t.Fatal(err)
	}
	if files := scaffoldedCertFiles(t, shared); len(files) != 1 {
		t.Fatalf("shared root: certs = %v, want one", files)
	}

	distinct := t.TempDir()
	s = &Scaffolder{TLSTrust: hostTrust(t, srv, []string{newTLSHost(t, srv, proxy), newTLSHost(t, srv, other)}, []testCA{proxy, other}, nil, false)}
	if err := s.Scaffold(distinct, Options{BuildTools: "generic", CACerts: []string{CACertAuto}}, &fakePrompter{confirm: true}); err != nil {
		t.Fatal(err)
	}
	if files := scaffoldedCertFiles(t, distinct); len(files) != 0 {
		t.Fatalf("distinct roots: certs = %v, want none", files)
	}
}

func TestScaffold_CACertAutoFailsWhenHostCannotVerify(t *testing.T) {
	srv := tlsHosts{}
	proxy := newTestCA(t, "Untrusted Proxy Root")
	dir := t.TempDir()

	s := &Scaffolder{TLSTrust: hostTrust(t, srv, []string{newTLSHost(t, srv, proxy)}, nil, nil, true)}
	err := s.Scaffold(dir, Options{BuildTools: "generic", CACerts: []string{CACertAuto}}, &fakePrompter{confirm: true})
	if err == nil || !strings.Contains(err.Error(), "--ca-cert <path>") {
		t.Fatalf("err = %v, want guidance to pass --ca-cert <path>", err)
	}
	if _, statErr := os.Stat(dir + "/.sandman/Dockerfile"); !os.IsNotExist(statErr) {
		t.Fatalf("scaffold wrote a Dockerfile despite probe failure")
	}
}

// bufferedConnPair returns a connected socket pair. Unlike net.Pipe its
// writes are buffered, so a client that aborts the handshake does not
// deadlock against the server's pending flight.
func bufferedConnPair() (net.Conn, net.Conn, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	conns := make([]net.Conn, 2)
	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "socketpair")
		conn, err := net.FileConn(f)
		f.Close()
		if err != nil {
			return nil, nil, err
		}
		conns[i] = conn
	}
	return conns[0], conns[1], nil
}
