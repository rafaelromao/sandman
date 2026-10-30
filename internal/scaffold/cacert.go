package scaffold

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/rafaelromao/sandman/internal/atomicfs"
)

// CACertAuto and CACertNone are the reserved --ca-cert values. Any other value
// is a path to a PEM file holding one or more certificates.
const (
	CACertAuto = "auto"
	CACertNone = "none"
)

// caCertsDirName is the .sandman/ subdirectory holding the trusted CA
// certificates baked into the sandbox image. The directory is the single
// source of truth: init writes it, and the Dockerfile header and drift check
// are derived from it.
const caCertsDirName = "certs"

// containerCABundle is the system bundle update-ca-certificates regenerates
// inside the Debian-based sandbox image.
const containerCABundle = "/etc/ssl/certs/ca-certificates.crt"

// caCertTrustEnv lists the variables pointed at containerCABundle so runtimes
// that ignore the system store (Node, Python requests, curl builds) trust the
// same certificates.
var caCertTrustEnv = []string{"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"}

func caCertsDir(sandmanDir string) string {
	return filepath.Join(sandmanDir, caCertsDirName)
}

// caCertFingerprint is the stable short identity used for file names and the
// Dockerfile metadata header.
func caCertFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])[:16]
}

func caCertFingerprints(certs []*x509.Certificate) []string {
	fps := make([]string, 0, len(certs))
	for _, cert := range certs {
		fps = append(fps, caCertFingerprint(cert))
	}
	sort.Strings(fps)
	return fps
}

// parsePEMCertificates returns every CERTIFICATE block in data. Input with no
// certificate, or with a certificate that does not parse, is rejected.
func parsePEMCertificates(data []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse certificate: %w", err)
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, errors.New("no PEM CERTIFICATE block found")
	}
	return certs, nil
}

// dedupeCACerts keeps the first occurrence of each certificate and returns
// them ordered by fingerprint so rendering is deterministic.
func dedupeCACerts(certs []*x509.Certificate) []*x509.Certificate {
	seen := map[string]bool{}
	out := make([]*x509.Certificate, 0, len(certs))
	for _, cert := range certs {
		fp := caCertFingerprint(cert)
		if seen[fp] {
			continue
		}
		seen[fp] = true
		out = append(out, cert)
	}
	sort.Slice(out, func(i, j int) bool { return caCertFingerprint(out[i]) < caCertFingerprint(out[j]) })
	return out
}

// readCACertDir loads the trusted certificates from .sandman/certs/. A missing
// directory means no certificates. Every *.crt file must hold exactly one
// valid PEM certificate.
func readCACertDir(dir string) ([]*x509.Certificate, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.crt"))
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}
	sort.Strings(files)
	var certs []*x509.Certificate
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read CA certificate %s: %w", file, err)
		}
		parsed, err := parsePEMCertificates(data)
		if err != nil {
			return nil, fmt.Errorf("invalid CA certificate %s: %w", file, err)
		}
		if len(parsed) != 1 {
			return nil, fmt.Errorf("invalid CA certificate %s: want exactly one certificate, found %d", file, len(parsed))
		}
		certs = append(certs, parsed[0])
	}
	return dedupeCACerts(certs), nil
}

// writeCACertDir replaces the certificate set atomically per file: each new
// certificate is written via temp file + rename before stale files are
// removed, so an interrupted write never leaves the directory empty.
func writeCACertDir(dir string, certs []*x509.Certificate) error {
	keep := map[string]bool{}
	if len(certs) > 0 {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	for _, cert := range certs {
		name := caCertFingerprint(cert) + ".crt"
		keep[name] = true
		data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
		if err := atomicfs.WriteAtomic(filepath.Join(dir, name), data, 0644); err != nil {
			return fmt.Errorf("write CA certificate %s: %w", name, err)
		}
	}
	stale, err := filepath.Glob(filepath.Join(dir, "*.crt"))
	if err != nil {
		return fmt.Errorf("list %s: %w", dir, err)
	}
	for _, file := range stale {
		if keep[filepath.Base(file)] {
			continue
		}
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale CA certificate %s: %w", file, err)
		}
	}
	return nil
}

// caCertSelection is the resolved --ca-cert request: either keep the current
// directory contents, or replace them with certs (empty for "none").
type caCertSelection struct {
	replace bool
	certs   []*x509.Certificate
	// report is printed once the selection has been written.
	report string
}

// parseCACertValues validates the raw --ca-cert values without touching the
// filesystem beyond reading the named PEM files.
func parseCACertValues(values []string) (paths []string, auto bool, none bool, err error) {
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		switch value {
		case "":
			return nil, false, false, errors.New("--ca-cert value must not be empty")
		case CACertNone:
			none = true
		case CACertAuto:
			auto = true
		default:
			if !slices.Contains(paths, value) {
				paths = append(paths, value)
			}
		}
	}
	if none && (auto || len(paths) > 0) {
		return nil, false, false, errors.New("--ca-cert none cannot be combined with other --ca-cert values")
	}
	return paths, auto, none, nil
}

func readCACertFile(path string) ([]*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read --ca-cert %s: %w", path, err)
	}
	certs, err := parsePEMCertificates(data)
	if err != nil {
		return nil, fmt.Errorf("invalid --ca-cert %s: %w", path, err)
	}
	return certs, nil
}

// renderCACertMetadataLine is the Dockerfile header recording the installed
// certificate set; empty when no certificates are trusted so the no-cert
// Dockerfile stays byte-identical to the historical output.
func renderCACertMetadataLine(fingerprints []string) string {
	if len(fingerprints) == 0 {
		return ""
	}
	return "# sandman ca-certs: " + strings.Join(fingerprints, ",") + "\n"
}

// renderCACertInstall installs the trusted certificates right after the apt
// step, before any HTTPS download.
func renderCACertInstall(fingerprints []string) string {
	if len(fingerprints) == 0 {
		return ""
	}
	var out bytes.Buffer
	fmt.Fprintf(&out, "COPY .sandman/%s/ /usr/local/share/ca-certificates/sandman/\n", caCertsDirName)
	out.WriteString("RUN update-ca-certificates\n")
	for _, name := range caCertTrustEnv {
		fmt.Fprintf(&out, "ENV %s=%q\n", name, containerCABundle)
	}
	return out.String()
}

// resolveCACerts turns the --ca-cert values into the certificate set the
// scaffold should trust. No values keeps (and validates) the current
// .sandman/certs/ contents.
func (s *Scaffolder) resolveCACerts(sandmanDir string, values []string) (caCertSelection, error) {
	if len(values) == 0 {
		existing, err := readCACertDir(caCertsDir(sandmanDir))
		if err != nil {
			return caCertSelection{}, err
		}
		return caCertSelection{certs: existing}, nil
	}
	paths, auto, none, err := parseCACertValues(values)
	if err != nil {
		return caCertSelection{}, err
	}
	if none {
		return caCertSelection{replace: true}, nil
	}
	var certs []*x509.Certificate
	for _, path := range paths {
		parsed, err := readCACertFile(path)
		if err != nil {
			return caCertSelection{}, err
		}
		certs = append(certs, parsed...)
	}
	report := ""
	if auto {
		detected, detectReport, err := s.TLSTrust.detectInterceptionRoots()
		if err != nil {
			return caCertSelection{}, err
		}
		report = detectReport
		if len(detected) == 0 && len(paths) == 0 {
			kept, err := s.resolveCACerts(sandmanDir, nil)
			kept.report = report
			return kept, err
		}
		certs = append(certs, detected...)
	}
	return caCertSelection{replace: true, certs: dedupeCACerts(certs), report: report}, nil
}

// validateCACertMetadata reports drift between the trusted certificates in
// .sandman/certs/ and the set the Dockerfile installs.
func validateCACertMetadata(sandmanDir string, dockerfileFingerprints []string) error {
	certs, err := readCACertDir(caCertsDir(sandmanDir))
	if err != nil {
		return fmt.Errorf("scaffold metadata drift: %w; fix .sandman/certs/ and run `sandman init`", err)
	}
	want := caCertFingerprints(certs)
	got := slices.Clone(dockerfileFingerprints)
	sort.Strings(got)
	if slices.Equal(want, got) {
		return nil
	}
	return fmt.Errorf("scaffold metadata drift: Dockerfile ca-certs %v does not match .sandman/certs/ %v; run `sandman init` to regenerate .sandman/Dockerfile", got, want)
}
