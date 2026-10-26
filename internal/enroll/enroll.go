// Package enroll turns a one-time code from the CMS into a device certificate.
//
//  1. POST {server}/device-api/enroll {code}            -> device uuid + polling token
//  2. generate the device key here (it never leaves the PC) and a CSR with CN = uuid
//  3. POST {server}/device-api/enroll/{uuid}/csr         -> the CMS's view of the key fingerprint
//  4. show the fingerprint so the admin can compare it before approving
//  5. GET  {server}/device-api/enroll/{uuid} until approved -> certificate, CA certificate, display URL
//  6. write device.key, device.crt, ca.crt and config.json
//
// Safe to run again. The code works once, so after step 1 the uuid, token and new key are kept in
// enroll-pending.json / enroll-pending.key: running enroll again (after Ctrl-C, a power cut, a lost
// SSH session) resumes waiting with the same key and fingerprint instead of needing a new code. A PC
// that is already enrolled is left alone unless Force is set, and even then its working certificate
// stays in place until the new one is approved.
//
// Phase 0 moves the key into the TPM; for now it is a root-only file.
package enroll

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Options configures one enrollment.
type Options struct {
	Server       string // the normal CMS host, e.g. https://cms.example.com
	Code         string // may be empty when resuming an enrollment in progress
	Dir          string // where device.key, device.crt, ca.crt and config.json go
	Root         string // the agent's install root, written into config.json
	Force        bool   // enroll again although this PC already has a certificate
	PollInterval time.Duration
	Timeout      time.Duration
	HTTP         *http.Client
	Out          io.Writer
}

// ErrEnrolled: the PC already has a certificate and config, and Force isn't set.
var ErrEnrolled = errors.New("this PC is already enrolled")

const (
	pendingFile    = "enroll-pending.json"
	pendingKeyFile = "enroll-pending.key"
)

// pending is an enrollment that has used its code but isn't approved yet.
type pending struct {
	Server     string `json:"server"`
	CodeHash   string `json:"code_hash"`
	DeviceUUID string `json:"device_uuid"`
	DeviceName string `json:"device_name"`
	Token      string `json:"token"`
	CSRSent    bool   `json:"csr_sent"`
}

// Enrolled reports whether dir holds a finished enrollment (config, certificate and key).
func Enrolled(dir string) bool {
	for _, name := range []string{"config.json", "device.crt", "device.key"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return false
		}
	}
	return true
}

// InProgress reports whether dir holds an enrollment that can be resumed without a code.
func InProgress(dir string) bool {
	p, _, err := loadPending(dir)
	return err == nil && p != nil
}

// Run enrolls the device and writes its files. It returns once the admin has
// approved the request in the CMS, or fails.
func Run(ctx context.Context, o Options) error {
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if o.PollInterval == 0 {
		o.PollInterval = 3 * time.Second
	}
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Minute
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	server := strings.TrimRight(o.Server, "/")
	if !strings.HasPrefix(server, "https://") {
		return errors.New("--server must be an https URL")
	}
	if Enrolled(o.Dir) && !o.Force {
		return fmt.Errorf("%w (%s); to enroll again: revoke it in the CMS, issue a new code, run enroll with --force", ErrEnrolled, o.Dir)
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return err
	}

	p, key, err := loadPending(o.Dir)
	if err != nil {
		return err
	}
	// A new code, or another CMS, starts over; the same code (or none) resumes.
	if p != nil && (p.Server != server || (o.Code != "" && p.CodeHash != hashCode(o.Code))) {
		removePending(o.Dir)
		p, key = nil, nil
	}
	if p == nil {
		if o.Code == "" {
			return errors.New("--code is required (no enrollment in progress to resume)")
		}
		if p, key, err = begin(ctx, o, server); err != nil {
			return err
		}
		fmt.Fprintf(o.Out, "Enrolling as %q (%s)\n", p.DeviceName, p.DeviceUUID)
	} else {
		fmt.Fprintf(o.Out, "Resuming the enrollment in progress as %q (%s)\n", p.DeviceName, p.DeviceUUID)
	}
	localFP, err := KeyFingerprint(key.Public())
	if err != nil {
		return err
	}

	if !p.CSRSent {
		csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
			Subject: pkix.Name{CommonName: p.DeviceUUID},
		}, key)
		if err != nil {
			return err
		}
		var submitted struct {
			KeyFingerprint string `json:"key_fingerprint"`
		}
		csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
		status, err := call(ctx, o.HTTP, http.MethodPost, server+"/device-api/enroll/"+p.DeviceUUID+"/csr", p.Token,
			map[string]string{"csr": string(csrPEM)}, &submitted)
		switch {
		case err == nil && submitted.KeyFingerprint != localFP:
			removePending(o.Dir)
			return fmt.Errorf("the CMS saw key %s but this PC has %s: something is in between, stopping", submitted.KeyFingerprint, localFP)
		case err == nil, status == http.StatusUnprocessableEntity && strings.Contains(err.Error(), "already sent"):
			// Sent now, or by an earlier run that stopped before it could note it. The
			// certificate is checked against this key below either way.
			p.CSRSent = true
			if err := savePending(o.Dir, p, nil); err != nil {
				return err
			}
		default:
			return fmt.Errorf("certificate request: %w", abandonIfRefused(o.Dir, status, err))
		}
	}
	fmt.Fprintf(o.Out, "\n  Key fingerprint:  %s\n\n  Approve this device in the CMS only if it shows the same fingerprint.\n  Waiting for approval...\n", localFP)

	var approved struct {
		Status        string `json:"status"`
		Certificate   string `json:"certificate"`
		CACertificate string `json:"ca_certificate"`
		DisplayURL    string `json:"display_url"`
	}
	deadline := time.Now().Add(o.Timeout)
	for {
		status, err := call(ctx, o.HTTP, http.MethodGet, server+"/device-api/enroll/"+p.DeviceUUID, p.Token, nil, &approved)
		if err != nil && status/100 == 4 {
			return fmt.Errorf("waiting for approval: %w", abandonIfRefused(o.Dir, status, err))
		}
		if err != nil {
			// Network or server trouble: keep waiting, the enrollment is still valid.
			fmt.Fprintf(o.Out, "  (can't reach the CMS right now: %v; still waiting)\n", err)
		} else if status == http.StatusOK && approved.Status == "approved" {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("not approved yet; run enroll again to keep waiting (no new code needed)")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w; run enroll again to keep waiting (no new code needed)", ctx.Err())
		case <-time.After(o.PollInterval):
		}
	}
	if err := checkCertificate(approved.Certificate, key, p.DeviceUUID); err != nil {
		removePending(o.Dir)
		return err
	}

	// The pending files go last: if this stops halfway, running enroll again fetches the same
	// certificate and writes everything again.
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	keyPath := filepath.Join(o.Dir, "device.key")
	certPath := filepath.Join(o.Dir, "device.crt")
	config, err := json.MarshalIndent(map[string]any{
		"cms_url":   approved.DisplayURL,
		"cert_file": certPath,
		"key_file":  keyPath,
		"root":      o.Root,
	}, "", "  ")
	if err != nil {
		return err
	}
	for _, f := range []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{filepath.Join(o.Dir, "ca.crt"), []byte(approved.CACertificate), 0o644},
		{keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600},
		{certPath, []byte(approved.Certificate), 0o644},
		{filepath.Join(o.Dir, "config.json"), append(config, '\n'), 0o644},
	} {
		if err := writeFile(f.path, f.data, f.mode); err != nil {
			return err
		}
	}
	removePending(o.Dir)
	fmt.Fprintf(o.Out, "Approved. Certificate and config written to %s; display host %s\n", o.Dir, approved.DisplayURL)
	return nil
}

// begin trades the code for a uuid and polling token and makes the key. Both are saved before
// anything else is sent, so the code isn't lost if the run stops here.
func begin(ctx context.Context, o Options, server string) (*pending, *ecdsa.PrivateKey, error) {
	var begun struct {
		DeviceUUID string `json:"device_uuid"`
		DeviceName string `json:"device_name"`
		Token      string `json:"token"`
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	if _, err := call(ctx, o.HTTP, http.MethodPost, server+"/device-api/enroll", "", map[string]string{"code": o.Code}, &begun); err != nil {
		return nil, nil, fmt.Errorf("enrollment code: %w", err)
	}
	p := &pending{Server: server, CodeHash: hashCode(o.Code), DeviceUUID: begun.DeviceUUID, DeviceName: begun.DeviceName, Token: begun.Token}
	if err := savePending(o.Dir, p, key); err != nil {
		return nil, nil, err
	}
	return p, key, nil
}

// abandonIfRefused drops the enrollment in progress when the CMS refused it for good (revoked, a
// new code issued, unknown device): resuming can't work, only a new code can.
func abandonIfRefused(dir string, status int, err error) error {
	if status/100 != 4 {
		return err
	}
	removePending(dir)
	return fmt.Errorf("%w; the CMS no longer accepts this enrollment: issue a new code and run enroll again", err)
}

func loadPending(dir string) (*pending, *ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(filepath.Join(dir, pendingFile))
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var p pending
	keyPEM, keyErr := os.ReadFile(filepath.Join(dir, pendingKeyFile))
	if json.Unmarshal(data, &p) != nil || keyErr != nil {
		removePending(dir) // unreadable: start over with a code
		return nil, nil, nil
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		removePending(dir)
		return nil, nil, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	key, ok := parsed.(*ecdsa.PrivateKey)
	if err != nil || !ok {
		removePending(dir)
		return nil, nil, nil
	}
	return &p, key, nil
}

// savePending writes the state, and the key when given (it only changes in begin).
func savePending(dir string, p *pending, key *ecdsa.PrivateKey) error {
	if key != nil {
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return err
		}
		if err := writeFile(filepath.Join(dir, pendingKeyFile), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return err
		}
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, pendingFile), data, 0o600) // holds the polling token
}

func removePending(dir string) {
	os.Remove(filepath.Join(dir, pendingFile))
	os.Remove(filepath.Join(dir, pendingKeyFile))
}

// hashCode identifies the code an enrollment started with without keeping the code itself.
func hashCode(code string) string {
	norm := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			return r
		}
		return -1
	}, code)
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}

// KeyFingerprint is SHA-256 of the DER public key, first 16 bytes as
// "AB12 CD34 ...". The CMS computes the same (DeviceEnrollmentService::keyFingerprint).
func KeyFingerprint(pub crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	h := strings.ToUpper(hex.EncodeToString(sum[:16]))
	groups := make([]string, 0, 8)
	for i := 0; i < len(h); i += 4 {
		groups = append(groups, h[i:i+4])
	}
	return strings.Join(groups, " "), nil
}

// checkCertificate makes sure the CMS signed our key, for our uuid, for client auth.
func checkCertificate(certPEM string, key *ecdsa.PrivateKey, uuid string) error {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return errors.New("the CMS returned no certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(key.Public()) {
		return errors.New("the certificate is not for this PC's key")
	}
	if cert.Subject.CommonName != uuid {
		return fmt.Errorf("the certificate is for %q, not %q", cert.Subject.CommonName, uuid)
	}
	for _, usage := range cert.ExtKeyUsage {
		if usage == x509.ExtKeyUsageClientAuth {
			return nil
		}
	}
	return errors.New("the certificate is not valid for client authentication")
}

// call does one JSON request. Non-2xx answers become errors carrying the CMS's message.
func call(ctx context.Context, c *http.Client, method, url, token string, body, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode/100 != 2 {
		var msg struct {
			Message string `json:"message"`
		}
		json.Unmarshal(data, &msg)
		if msg.Message == "" {
			msg.Message = http.StatusText(resp.StatusCode)
		}
		return resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg.Message)
	}
	return resp.StatusCode, json.Unmarshal(data, out)
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
