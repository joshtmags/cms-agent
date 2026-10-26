package enroll

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCMS implements the enrollment endpoints with an in-memory CA.
type fakeCMS struct {
	t        *testing.T
	caKey    *ecdsa.PrivateKey
	caCert   *x509.Certificate
	caPEM    []byte
	csr      *x509.CertificateRequest
	approved bool
	// tamperFP makes the CMS report a different key fingerprint than it received.
	tamperFP bool
	// wrongKey makes the CMS sign a different key than the one in the CSR.
	wrongKey bool
	// hold keeps the device waiting for approval; refuse answers the poll with 403 (revoked).
	hold, refuse bool
	begins       int // the code works once, like the real CMS
}

func newFakeCMS(t *testing.T) (*fakeCMS, *httptest.Server) {
	f := &fakeCMS{t: t}
	f.caKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &f.caKey.PublicKey, f.caKey)
	f.caCert, _ = x509.ParseCertificate(der)
	f.caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	mux := http.NewServeMux()
	mux.HandleFunc("POST /device-api/enroll", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["code"] != "ABCD-EFGH" || f.begins > 0 {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"message": "Unknown or expired enrollment code."})
			return
		}
		f.begins++
		json.NewEncoder(w).Encode(map[string]string{"device_uuid": "dev-1", "device_name": "TV 1", "token": "tok"})
	})
	mux.HandleFunc("POST /device-api/enroll/dev-1/csr", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if f.csr != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]string{"message": "This device already sent its certificate request."})
			return
		}
		block, _ := pem.Decode([]byte(body["csr"]))
		f.csr, _ = x509.ParseCertificateRequest(block.Bytes)
		fp, _ := KeyFingerprint(f.csr.PublicKey)
		if f.tamperFP {
			fp = "0000 0000 0000 0000 0000 0000 0000 0000"
		}
		json.NewEncoder(w).Encode(map[string]string{"key_fingerprint": fp})
	})
	mux.HandleFunc("GET /device-api/enroll/dev-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || f.refuse {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if !f.approved || f.hold {
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
			f.approved = true // approve on the second poll
			return
		}
		pub := f.csr.PublicKey
		if f.wrongKey {
			other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			pub = &other.PublicKey
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: f.csr.Subject,
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
		der, _ := x509.CreateCertificate(rand.Reader, leaf, f.caCert, pub, f.caKey)
		json.NewEncoder(w).Encode(map[string]string{
			"status":         "approved",
			"certificate":    string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
			"ca_certificate": string(f.caPEM),
			"display_url":    "https://display.example.test",
		})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

func options(srv *httptest.Server, dir, code string) Options {
	return Options{Server: srv.URL, Code: code, Dir: dir, Root: "/opt/cms-arena-agent",
		PollInterval: time.Millisecond, Timeout: 5 * time.Second, HTTP: srv.Client()}
}

// interrupted runs an enrollment that stops while waiting for approval.
func interrupted(t *testing.T, f *fakeCMS, o Options) {
	t.Helper()
	f.hold = true
	o.Timeout = 20 * time.Millisecond
	if err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "run enroll again") {
		t.Fatalf("interrupted run: err = %v", err)
	}
	f.hold = false
}

func TestEnrollWritesKeyCertAndConfig(t *testing.T) {
	_, srv := newFakeCMS(t)
	dir := t.TempDir()
	if err := Run(context.Background(), options(srv, dir, "ABCD-EFGH")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "device.key"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("device.key mode = %v, %v", info.Mode(), err)
	}
	var cfg map[string]string
	data, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	json.Unmarshal(data, &cfg)
	if cfg["cms_url"] != "https://display.example.test" || cfg["cert_file"] != filepath.Join(dir, "device.crt") {
		t.Fatalf("config = %v", cfg)
	}
}

func TestEnrollRejectsUnknownCode(t *testing.T) {
	_, srv := newFakeCMS(t)
	err := Run(context.Background(), options(srv, t.TempDir(), "WRON-GCOD"))
	if err == nil || !strings.Contains(err.Error(), "Unknown or expired") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnrollStopsWhenTheFingerprintDiffers(t *testing.T) {
	f, srv := newFakeCMS(t)
	f.tamperFP = true
	if err := Run(context.Background(), options(srv, t.TempDir(), "ABCD-EFGH")); err == nil {
		t.Fatal("continued although the CMS saw a different key")
	}
}

func TestEnrollRejectsACertificateForAnotherKey(t *testing.T) {
	f, srv := newFakeCMS(t)
	f.wrongKey = true
	dir := t.TempDir()
	if err := Run(context.Background(), options(srv, dir, "ABCD-EFGH")); err == nil {
		t.Fatal("accepted a certificate for a different key")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Fatal("wrote a config for a bad certificate")
	}
}

func TestKeyFingerprintFormat(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	fp, err := KeyFingerprint(key.Public())
	if err != nil || len(fp) != 39 || strings.Count(fp, " ") != 7 || strings.ToUpper(fp) != fp {
		t.Fatalf("fingerprint %q, %v", fp, err)
	}
}

func TestEnrollResumesWithoutANewCode(t *testing.T) {
	f, srv := newFakeCMS(t)
	dir := t.TempDir()
	interrupted(t, f, options(srv, dir, "ABCD-EFGH"))
	if !InProgress(dir) || Enrolled(dir) {
		t.Fatal("no enrollment in progress after the interruption")
	}
	sent, _ := KeyFingerprint(f.csr.PublicKey)

	// Again without a code, and again with the same (now used) code: both resume.
	for _, code := range []string{"", "abcd-efgh"} {
		interrupted(t, f, options(srv, dir, code))
	}
	if err := Run(context.Background(), options(srv, dir, "")); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if f.begins != 1 {
		t.Fatalf("used the code %d times", f.begins)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "device.key"))
	block, _ := pem.Decode(data)
	key, _ := x509.ParsePKCS8PrivateKey(block.Bytes)
	if fp, _ := KeyFingerprint(key.(*ecdsa.PrivateKey).Public()); fp != sent {
		t.Fatalf("device.key %s is not the key the CMS saw (%s)", fp, sent)
	}
	if InProgress(dir) || !Enrolled(dir) {
		t.Fatal("pending files left after approval")
	}
}

func TestEnrollLeavesAnEnrolledPCAlone(t *testing.T) {
	_, srv := newFakeCMS(t)
	dir := t.TempDir()
	if err := Run(context.Background(), options(srv, dir, "ABCD-EFGH")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "device.key"))
	err := Run(context.Background(), options(srv, dir, "ABCD-EFGH"))
	if !errors.Is(err, ErrEnrolled) {
		t.Fatalf("err = %v, want ErrEnrolled", err)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "device.key")); !bytes.Equal(before, after) {
		t.Fatal("device.key changed")
	}
}

func TestReenrollKeepsTheOldCertificateUntilApproved(t *testing.T) {
	f, srv := newFakeCMS(t)
	dir := t.TempDir()
	if err := Run(context.Background(), options(srv, dir, "ABCD-EFGH")); err != nil {
		t.Fatal(err)
	}
	oldKey, _ := os.ReadFile(filepath.Join(dir, "device.key"))
	oldCert, _ := os.ReadFile(filepath.Join(dir, "device.crt"))

	*f = fakeCMS{t: t, caKey: f.caKey, caCert: f.caCert, caPEM: f.caPEM} // a new code for the same device
	o := options(srv, dir, "ABCD-EFGH")
	o.Force = true
	interrupted(t, f, o)
	if k, _ := os.ReadFile(filepath.Join(dir, "device.key")); !bytes.Equal(k, oldKey) {
		t.Fatal("the working key was replaced before approval")
	}
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if c, _ := os.ReadFile(filepath.Join(dir, "device.crt")); bytes.Equal(c, oldCert) {
		t.Fatal("certificate not replaced after approval")
	}
}

func TestEnrollDropsARefusedEnrollment(t *testing.T) {
	f, srv := newFakeCMS(t)
	dir := t.TempDir()
	interrupted(t, f, options(srv, dir, "ABCD-EFGH"))
	f.refuse = true // revoked, or a new code issued meanwhile
	err := Run(context.Background(), options(srv, dir, ""))
	if err == nil || !strings.Contains(err.Error(), "issue a new code") {
		t.Fatalf("err = %v", err)
	}
	if InProgress(dir) {
		t.Fatal("kept an enrollment the CMS refused")
	}
}

func TestEnrollWithANewCodeStartsOver(t *testing.T) {
	f, srv := newFakeCMS(t)
	dir := t.TempDir()
	interrupted(t, f, options(srv, dir, "ABCD-EFGH"))
	err := Run(context.Background(), options(srv, dir, "WRON-GCOD"))
	if err == nil || !strings.Contains(err.Error(), "Unknown or expired") {
		t.Fatalf("err = %v", err)
	}
	if InProgress(dir) {
		t.Fatal("kept the old enrollment after a different code")
	}
}
