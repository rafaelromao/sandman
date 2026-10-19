package scaffold

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T, commonName string) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func writeTestFile(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readScaffoldDockerfile(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func scaffoldedCertFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, ".sandman", "certs", "*.crt"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

const caCertInstallLines = "COPY .sandman/certs/ /usr/local/share/ca-certificates/sandman/\nRUN update-ca-certificates\n"

func TestScaffold_CACertPathInstallsCertificateBeforeHTTPSDownloads(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, "Corp Inspection Root")

	if err := (&Scaffolder{}).Scaffold(dir, Options{BuildTools: "generic", CACerts: []string{writeTestFile(t, ca.pem)}}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	files := scaffoldedCertFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("certs = %v, want one", files)
	}
	got, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(ca.pem) {
		t.Fatalf("stored certificate differs from input")
	}
	dockerfile := readScaffoldDockerfile(t, dir)
	if !strings.Contains(dockerfile, "# sandman ca-certs: "+caCertFingerprint(ca.cert)+"\n") {
		t.Errorf("missing ca-certs header:\n%s", dockerfile)
	}
	install := strings.Index(dockerfile, caCertInstallLines)
	if install < 0 {
		t.Fatalf("missing certificate install:\n%s", dockerfile)
	}
	for _, env := range []string{"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"} {
		if !strings.Contains(dockerfile, "ENV "+env+"=\"/etc/ssl/certs/ca-certificates.crt\"\n") {
			t.Errorf("missing ENV %s", env)
		}
	}
	assertTrustBeforeHTTPS(t, dockerfile)
}

// assertTrustBeforeHTTPS fails when any RUN step that may reach the network
// over HTTPS appears before the certificate trust is installed.
func assertTrustBeforeHTTPS(t *testing.T, dockerfile string) {
	t.Helper()
	trustEnd := strings.Index(dockerfile, "ENV CURL_CA_BUNDLE=")
	if trustEnd < 0 {
		t.Fatalf("no trust ENV in Dockerfile")
	}
	for i, line := range strings.Split(dockerfile[:trustEnd], "\n") {
		if strings.HasPrefix(line, "RUN ") && !strings.HasPrefix(line, "RUN apt-get update") && line != "RUN update-ca-certificates" {
			t.Errorf("line %d runs before certificate trust: %s", i+1, line)
		}
	}
}

func TestScaffold_NoCACertsKeepsDockerfileUnchangedForEveryPreset(t *testing.T) {
	for _, preset := range KnownBuildToolsPresets {
		t.Run(preset, func(t *testing.T) {
			s := &Scaffolder{}
			p := BuildToolsPreset{Name: preset, BaseImage: "debian:bookworm-slim", SharedPackages: []string{"curl"}, MiseVersion: DefaultMISEVersion}
			without := s.renderBuildToolsDockerfile(p, "opencode", "1.0.0", "1.25.0", "8.0", "22", "3.12", "1.17", "27", "3.3", "1.80", "21", nil)
			if strings.Contains(without, "sandman ca-certs") || strings.Contains(without, "update-ca-certificates") {
				t.Fatalf("no-cert Dockerfile mentions CA certificates:\n%s", without)
			}
			ca := newTestCA(t, "Corp Root")
			with := s.renderBuildToolsDockerfile(p, "opencode", "1.0.0", "1.25.0", "8.0", "22", "3.12", "1.17", "27", "3.3", "1.80", "21", []string{caCertFingerprint(ca.cert)})
			assertTrustBeforeHTTPS(t, with)
			stripped := strings.Replace(with, renderCACertMetadataLine([]string{caCertFingerprint(ca.cert)}), "", 1)
			stripped = strings.Replace(stripped, renderCACertInstall([]string{caCertFingerprint(ca.cert)}), "", 1)
			if stripped != without {
				t.Fatalf("certificate trust changed more than its own lines\nwithout:\n%s\nwith:\n%s", without, with)
			}
		})
	}
}

func TestScaffold_CACertBundleIsSplitAndDeduped(t *testing.T) {
	dir := t.TempDir()
	a := newTestCA(t, "Root A")
	b := newTestCA(t, "Root B")
	bundle := append(append(append([]byte{}, a.pem...), b.pem...), a.pem...)

	if err := (&Scaffolder{}).Scaffold(dir, Options{BuildTools: "generic", CACerts: []string{writeTestFile(t, bundle), writeTestFile(t, b.pem)}}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	files := scaffoldedCertFiles(t, dir)
	if len(files) != 2 {
		t.Fatalf("certs = %v, want two distinct certificates", files)
	}
	want := []string{caCertFingerprint(a.cert), caCertFingerprint(b.cert)}
	sort.Strings(want)
	if !strings.Contains(readScaffoldDockerfile(t, dir), "# sandman ca-certs: "+strings.Join(want, ",")+"\n") {
		t.Fatalf("header does not list both fingerprints in order")
	}
}

func TestScaffold_InvalidCACertLeavesScaffoldUntouched(t *testing.T) {
	ca := newTestCA(t, "Corp Root")
	cases := map[string]string{
		"missing file": filepath.Join(t.TempDir(), "missing.pem"),
		"not PEM":      writeTestFile(t, []byte("not a certificate\n")),
		"no cert":      writeTestFile(t, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2, 3}})),
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s := &Scaffolder{}
			if err := s.Scaffold(dir, Options{BuildTools: "generic", CACerts: []string{writeTestFile(t, ca.pem)}}, &fakePrompter{confirm: true}); err != nil {
				t.Fatalf("initial scaffold: %v", err)
			}
			before := readScaffoldDockerfile(t, dir)
			config, err := os.ReadFile(filepath.Join(dir, ".sandman", "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}

			err = s.Scaffold(dir, Options{BuildTools: "go", CACerts: []string{path}}, &fakePrompter{confirm: true})
			if err == nil || !strings.Contains(err.Error(), "--ca-cert") {
				t.Fatalf("err = %v, want --ca-cert error", err)
			}
			if readScaffoldDockerfile(t, dir) != before {
				t.Fatalf("Dockerfile changed after invalid --ca-cert")
			}
			after, _ := os.ReadFile(filepath.Join(dir, ".sandman", "config.yaml"))
			if string(after) != string(config) {
				t.Fatalf("config changed after invalid --ca-cert")
			}
			if files := scaffoldedCertFiles(t, dir); len(files) != 1 {
				t.Fatalf("certs = %v, want original certificate kept", files)
			}
		})
	}
}

func TestScaffold_ReInitKeepsCACertsUntilNone(t *testing.T) {
	dir := t.TempDir()
	s := &Scaffolder{}
	ca := newTestCA(t, "Corp Root")
	if err := s.Scaffold(dir, Options{BuildTools: "generic", CACerts: []string{writeTestFile(t, ca.pem)}}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("initial scaffold: %v", err)
	}

	if err := s.Scaffold(dir, Options{BuildTools: "generic"}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("re-init: %v", err)
	}
	if !strings.Contains(readScaffoldDockerfile(t, dir), caCertInstallLines) {
		t.Fatalf("re-init without --ca-cert dropped trusted certificates")
	}
	if files := scaffoldedCertFiles(t, dir); len(files) != 1 {
		t.Fatalf("certs = %v after re-init", files)
	}

	if err := s.Scaffold(dir, Options{BuildTools: "generic", CACerts: []string{CACertNone}}, &fakePrompter{confirm: true}); err != nil {
		t.Fatalf("re-init --ca-cert none: %v", err)
	}
	if files := scaffoldedCertFiles(t, dir); len(files) != 0 {
		t.Fatalf("certs = %v after --ca-cert none", files)
	}
	if strings.Contains(readScaffoldDockerfile(t, dir), "update-ca-certificates") {
		t.Fatalf("Dockerfile still trusts certificates after --ca-cert none")
	}
}

func TestScaffold_CACertNoneCannotBeCombined(t *testing.T) {
	ca := newTestCA(t, "Corp Root")
	err := (&Scaffolder{}).Scaffold(t.TempDir(), Options{BuildTools: "generic", CACerts: []string{CACertNone, writeTestFile(t, ca.pem)}}, &fakePrompter{confirm: true})
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("err = %v, want combination rejection", err)
	}
}

func TestScaffold_InvalidFileInCertsDirFailsReInit(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".sandman", "certs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".sandman", "certs", "stray.crt"), []byte("junk"), 0644); err != nil {
		t.Fatal(err)
	}
	err := (&Scaffolder{}).Scaffold(dir, Options{BuildTools: "generic"}, &fakePrompter{confirm: true})
	if err == nil || !strings.Contains(err.Error(), "stray.crt") {
		t.Fatalf("err = %v, want invalid certs-dir file named", err)
	}
}
