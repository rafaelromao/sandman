package scaffold

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// defaultCACertProbeHosts are HTTPS hosts the scaffolded image downloads from
// while building. `--ca-cert auto` probes them to find a TLS-inspection root.
var defaultCACertProbeHosts = []string{"mise.run", "github.com", "registry.npmjs.org"}

const caCertProbeTimeout = 15 * time.Second

// TLSTrust is the host-side seam `sandman init --ca-cert auto` uses to find a
// TLS-inspection proxy's root certificate. Zero fields fall back to the
// production defaults.
type TLSTrust struct {
	// Hosts are probed over HTTPS (host or host:port).
	Hosts []string
	// Transport performs the probe with host trust. The default honors
	// HTTPS_PROXY and the host's system trust store.
	Transport http.RoundTripper
	// PublicRoots returns the platform's public root set; ok is false when
	// the platform offers none.
	PublicRoots func() (roots []*x509.Certificate, ok bool)
}

func (t TLSTrust) hosts() []string {
	if len(t.Hosts) > 0 {
		return t.Hosts
	}
	return defaultCACertProbeHosts
}

func (t TLSTrust) transport() http.RoundTripper {
	if t.Transport != nil {
		return t.Transport
	}
	return &http.Transport{Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: caCertProbeTimeout, DisableKeepAlives: true}
}

func (t TLSTrust) publicRoots() ([]*x509.Certificate, bool) {
	if t.PublicRoots != nil {
		return t.PublicRoots()
	}
	return platformPublicRoots()
}

// verifiedRoot returns the root of the chain the host verified for host.
func (t TLSTrust) verifiedRoot(transport http.RoundTripper, host string) (*x509.Certificate, error) {
	ctx, cancel := context.WithTimeout(context.Background(), caCertProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://"+host+"/", nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.TLS == nil || len(resp.TLS.VerifiedChains) == 0 || len(resp.TLS.VerifiedChains[0]) == 0 {
		return nil, errors.New("no verified certificate chain")
	}
	chain := resp.TLS.VerifiedChains[0]
	return chain[len(chain)-1], nil
}

// detectInterceptionRoots probes every host and returns the roots that show
// TLS interception: roots outside the platform's public set or, when no
// public set is available, a single root shared by every probed host (a
// heuristic, reported as such). The report describes the outcome for the
// caller to print once the certificates are written.
func (t TLSTrust) detectInterceptionRoots() ([]*x509.Certificate, string, error) {
	hosts := t.hosts()
	transport := t.transport()
	if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
		defer closer.CloseIdleConnections()
	}
	roots := make([]*x509.Certificate, 0, len(hosts))
	for _, host := range hosts {
		root, err := t.verifiedRoot(transport, host)
		if err != nil {
			return nil, "", fmt.Errorf("--ca-cert auto: this host cannot verify https://%s (%v); export your TLS-inspection proxy's root certificate and pass --ca-cert <path>", host, err)
		}
		roots = append(roots, root)
	}

	var captured []*x509.Certificate
	reason := "not a public root of this operating system"
	if public, ok := t.publicRoots(); ok {
		known := map[string]bool{}
		for _, cert := range public {
			known[rootIdentity(cert)] = true
		}
		for _, root := range roots {
			if !known[rootIdentity(root)] {
				captured = append(captured, root)
			}
		}
	} else if len(roots) > 1 && len(dedupeCACerts(roots)) == 1 {
		captured = roots[:1]
		reason = "shared by every probed host; no public root list is available here to confirm it is a proxy"
	}
	captured = dedupeCACerts(captured)

	if len(captured) == 0 {
		return nil, "--ca-cert auto: no TLS interception detected; trusted CA certificates unchanged.\n", nil
	}
	var report strings.Builder
	for _, cert := range captured {
		fmt.Fprintf(&report, "--ca-cert auto: trusting TLS-inspection root %q (%s): %s.\n", certDisplayName(cert), caCertFingerprint(cert), reason)
	}
	return captured, report.String(), nil
}

// rootIdentity matches a root by subject and public key rather than raw
// bytes, so a re-issued copy of a public root still counts as public.
func rootIdentity(cert *x509.Certificate) string {
	sum := sha256.Sum256(append(append([]byte{}, cert.RawSubject...), cert.RawSubjectPublicKeyInfo...))
	return hex.EncodeToString(sum[:])
}

func certDisplayName(cert *x509.Certificate) string {
	if cert.Subject.CommonName != "" {
		return cert.Subject.CommonName
	}
	return cert.Subject.String()
}

// platformPublicRoots reads the operating system's public root set, which
// excludes certificates an administrator added (such as a corporate proxy
// root).
func platformPublicRoots() ([]*x509.Certificate, bool) {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("security", "find-certificate", "-a", "-p", "/System/Library/Keychains/SystemRootCertificates.keychain").Output()
		if err != nil {
			return nil, false
		}
		certs, err := parsePEMCertificates(out)
		if err != nil {
			return nil, false
		}
		return certs, true
	case "linux":
		files, _ := filepath.Glob("/usr/share/ca-certificates/mozilla/*.crt")
		var certs []*x509.Certificate
		for _, file := range files {
			data, err := os.ReadFile(file)
			if err != nil {
				continue
			}
			if parsed, err := parsePEMCertificates(data); err == nil {
				certs = append(certs, parsed...)
			}
		}
		return certs, len(certs) > 0
	}
	return nil, false
}
