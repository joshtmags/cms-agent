// Package enroll turns a one-time code from the CMS into a device certificate.
//
//  1. POST {server}/device-api/enroll {code}            -> device uuid + polling token
//  2. generate the device key here (it never leaves the PC) and a CSR with CN = uuid
//  3. POST {server}/device-api/enroll/{uuid}/csr         -> the CMS's view of the key fingerprint
//  4. show the fingerprint so the admin can compare it before approving
//  5. GET  {server}/device-api/enroll/{uuid} until approved -> certificate, CA certificate, display URL
//  6. write device.key, device.crt, ca.crt and config.json
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
	Code         string
	Dir          string // where device.key, device.crt, ca.crt and config.json go
	Root         string // the agent's install root, written into config.json
	PollInterval time.Duration
	Timeout      time.Duration
	HTTP         *http.Client
	Out          io.Writer
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
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return err
	}

	var begun struct {
		DeviceUUID string `json:"device_uuid"`
		DeviceName string `json:"device_name"`
		Token      string `json:"token"`
	}
	if _, err := call(ctx, o.HTTP, http.MethodPost, server+"/device-api/enroll", "", map[string]string{"code": o.Code}, &begun); err != nil {
		return fmt.Errorf("enrollment code: %w", err)
	}
	fmt.Fprintf(o.Out, "Enrolling as %q (%s)\n", begun.DeviceName, begun.DeviceUUID)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	keyPath := filepath.Join(o.Dir, "device.key")
	if err := writeFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: begun.DeviceUUID},
	}, key)
	if err != nil {
		return err
	}
	localFP, err := KeyFingerprint(key.Public())
	if err != nil {
		return err
	}

	var submitted struct {
		KeyFingerprint string `json:"key_fingerprint"`
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	if _, err := call(ctx, o.HTTP, http.MethodPost, server+"/device-api/enroll/"+begun.DeviceUUID+"/csr", begun.Token,
		map[string]string{"csr": string(csrPEM)}, &submitted); err != nil {
		return fmt.Errorf("certificate request: %w", err)
	}
	if submitted.KeyFingerprint != localFP {
		return fmt.Errorf("the CMS saw key %s but this PC has %s: something is in between, stopping", submitted.KeyFingerprint, localFP)
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
		status, err := call(ctx, o.HTTP, http.MethodGet, server+"/device-api/enroll/"+begun.DeviceUUID, begun.Token, nil, &approved)
		if err != nil {
			return fmt.Errorf("waiting for approval: %w", err)
		}
		if status == http.StatusOK && approved.Status == "approved" {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("not approved in time; issue a new code and run enroll again")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.PollInterval):
		}
	}
	if err := checkCertificate(approved.Certificate, key, begun.DeviceUUID); err != nil {
		return err
	}

	certPath := filepath.Join(o.Dir, "device.crt")
	if err := writeFile(certPath, []byte(approved.Certificate), 0o644); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(o.Dir, "ca.crt"), []byte(approved.CACertificate), 0o644); err != nil {
		return err
	}
	config, err := json.MarshalIndent(map[string]any{
		"cms_url":   approved.DisplayURL,
		"cert_file": certPath,
		"key_file":  keyPath,
		"root":      o.Root,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(o.Dir, "config.json"), append(config, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "Approved. Certificate and config written to %s; display host %s\n", o.Dir, approved.DisplayURL)
	return nil
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
