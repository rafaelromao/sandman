package cmd

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTestCAPEM(t *testing.T, path, commonName string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestInit_CACertFlagInstallsTrustedCertificateBeforeHTTPSDownloads(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	caPath := filepath.Join(t.TempDir(), "corp-ca.pem")
	writeTestCAPEM(t, caPath, "Corp Inspection Root")

	var out bytes.Buffer
	cmd := NewInitCmd()
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"--build-tools", "generic", "--ca-cert", caPath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init --ca-cert: %v", err)
	}

	certs, err := filepath.Glob(filepath.Join(dir, ".sandman", "certs", "*.crt"))
	if err != nil || len(certs) != 1 {
		t.Fatalf(".sandman/certs/*.crt = %v (err %v), want exactly one certificate", certs, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(data)
	if !strings.Contains(dockerfile, "# sandman ca-certs: ") {
		t.Errorf("Dockerfile missing ca-certs metadata header:\n%s", dockerfile)
	}
	install := strings.Index(dockerfile, "COPY .sandman/certs/ /usr/local/share/ca-certificates/sandman/\nRUN update-ca-certificates\n")
	mise := strings.Index(dockerfile, initMiseInstall)
	if install < 0 || mise < 0 || install > mise {
		t.Fatalf("certificate install (at %d) must precede mise download (at %d):\n%s", install, mise, dockerfile)
	}
	for _, env := range []string{"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"} {
		line := "ENV " + env + "=\"/etc/ssl/certs/ca-certificates.crt\"\n"
		if i := strings.Index(dockerfile, line); i < 0 || i > mise {
			t.Errorf("Dockerfile missing %q before mise download", line)
		}
	}
}
