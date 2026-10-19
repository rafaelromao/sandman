package scaffold

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	return &http.Transport{Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: caCertProbeTimeout}
}

func (t TLSTrust) publicRoots() ([]*x509.Certificate, bool) {
	if t.PublicRoots != nil {
		return t.PublicRoots()
	}
	return platformPublicRoots()
}

// verifiedRoot returns the root of the chain the host verified for host.
func (t TLSTrust) verifiedRoot(host string) (*x509.Certificate, error) {
	ctx, cancel := context.WithTimeout(context.Background(), caCertProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://"+host+"/", nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Transport: t.transport(), CheckRedirect: func(*http.Request, []*http.Request) error {
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
// public set is available, a single root shared by every probed host.
func (t TLSTrust) detectInterceptionRoots(w io.Writer) ([]*x509.Certificate, error) {
	hosts := t.hosts()
	roots := make([]*x509.Certificate, 0, len(hosts))
	for _, host := range hosts {
		root, err := t.verifiedRoot(host)
		if err != nil {
			return nil, fmt.Errorf("--ca-cert auto: this host cannot verify https://%s (%v); export your TLS-inspection proxy's root certificate and pass --ca-cert <path>", host, err)
		}
		roots = append(roots, root)
	}

	var captured []*x509.Certificate
	if public, ok := t.publicRoots(); ok {
		known := map[string]bool{}
		for _, cert := range public {
			known[caCertFingerprint(cert)] = true
		}
		for _, root := range roots {
			if !known[caCertFingerprint(root)] {
				captured = append(captured, root)
			}
		}
	} else if len(roots) > 1 && len(dedupeCACerts(roots)) == 1 {
		captured = roots[:1]
	}
	captured = dedupeCACerts(captured)

	if len(captured) == 0 {
		fmt.Fprintln(w, "--ca-cert auto: no TLS interception detected; trusted CA certificates unchanged.")
		return nil, nil
	}
	for _, cert := range captured {
		fmt.Fprintf(w, "--ca-cert auto: trusting TLS-inspection root %q (%s).\n", cert.Subject.CommonName, caCertFingerprint(cert))
	}
	return captured, nil
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
