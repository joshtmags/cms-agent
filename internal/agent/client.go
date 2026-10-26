package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// State is the CMS's answer to GET /device-api/v1/state (display layout goes
// to the page, not the agent).
type State struct {
	Agent    AgentState `json:"agent"`
	Commands []Command  `json:"commands"`
}

// Command is something the admin asked for: restart_browser, reboot,
// shutdown or screenshot. Each is handed out once and reported back.
type Command struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// AgentState tells the agent which version it should run.
type AgentState struct {
	DesiredVersion string `json:"desired_version"`
	// BundleURL is the base URL holding manifest.json, manifest.json.minisig
	// and the tarball, e.g. https://display.example.com/device-api/v1/bundles/1.1.0
	BundleURL string `json:"bundle_url"`
	// SafeToUpdate is false while a fight event is ongoing or a draw is open.
	SafeToUpdate bool `json:"safe_to_update"`
}

// Heartbeat is POSTed to /device-api/v1/heartbeat after every state poll.
type Heartbeat struct {
	AgentVersion   string            `json:"agent_version"`
	Hostname       string            `json:"hostname"`
	UptimeSeconds  int64             `json:"uptime_seconds"`
	PendingUpdate  string            `json:"pending_update,omitempty"`
	FailedVersions map[string]string `json:"failed_versions,omitempty"`
	LastUpdate     *UpdateResult     `json:"last_update,omitempty"`
}

// UpdateResult reports the outcome of the most recent update attempt.
type UpdateResult struct {
	Version string    `json:"version"`
	Status  string    `json:"status"` // waiting, rejected, failed, installed, healthy, rolled_back
	Error   string    `json:"error,omitempty"`
	At      time.Time `json:"at"`
}

// Client talks to the display host over mTLS.
type Client struct {
	base string
	http *http.Client
}

// NewClient builds an mTLS client with the device certificate. The server is
// verified against the system roots (the display host has a public
// certificate).
func NewClient(c *Config) (*Client, error) {
	certs := &certFiles{cert: c.CertFile, key: c.KeyFile}
	if _, err := certs.get(nil); err != nil {
		return nil, fmt.Errorf("device certificate: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		GetClientCertificate: certs.get,
		MinVersion:           tls.VersionTLS12,
	}
	return &Client{
		base: strings.TrimRight(c.CMSURL, "/"),
		http: &http.Client{Transport: transport, Timeout: 30 * time.Second},
	}, nil
}

// certFiles reloads the certificate whenever the files change, so a renewed
// or re-enrolled certificate is used without restarting the agent.
type certFiles struct {
	cert, key string
	mu        sync.Mutex
	loaded    *tls.Certificate
	modTime   time.Time
}

func (c *certFiles) get(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	info, err := os.Stat(c.cert)
	if err != nil {
		return nil, err
	}
	if c.loaded != nil && info.ModTime().Equal(c.modTime) {
		return c.loaded, nil
	}
	cert, err := tls.LoadX509KeyPair(c.cert, c.key)
	if err != nil {
		return nil, err
	}
	c.loaded, c.modTime = &cert, info.ModTime()
	return c.loaded, nil
}

// State fetches the desired state.
func (c *Client) State(ctx context.Context) (*State, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/device-api/v1/state", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusLocked {
		// Halted in the CMS: same certificate, so polling on picks up again on Resume.
		return nil, errors.New("halted by the administrator (HTTP 423); waiting for Resume")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var st State
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil {
		return nil, fmt.Errorf("decoding: %w", err)
	}
	return &st, nil
}

// SendHeartbeat reports the agent's status.
func (c *Client) SendHeartbeat(ctx context.Context, hb Heartbeat) error {
	body, err := json.Marshal(hb)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/device-api/v1/heartbeat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// ReportCommand tells the CMS how a command went.
func (c *Client) ReportCommand(ctx context.Context, id int64, ok bool, errText string) error {
	body, err := json.Marshal(map[string]any{"ok": ok, "error": errText})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/device-api/v1/commands/%d/result", c.base, id), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return c.expectNoContent(req)
}

// UploadScreenshot sends a PNG for a screenshot command.
func (c *Client) UploadScreenshot(ctx context.Context, id int64, pngPath string) error {
	f, err := os.Open(pngPath)
	if err != nil {
		return err
	}
	defer f.Close()
	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	part, err := form.CreateFormFile("screenshot", "screenshot.png")
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, io.LimitReader(f, 8<<20)); err != nil {
		return err
	}
	if err := form.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/device-api/v1/commands/%d/screenshot", c.base, id), &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	return c.expectNoContent(req)
}

func (c *Client) expectNoContent(req *http.Request) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// Download saves url to dest, refusing anything larger than limit bytes.
// The bundle URL must be on the display host, so downloads also use mTLS.
func (c *Client) Download(ctx context.Context, url, dest string, limit int64) error {
	if !strings.HasPrefix(url, c.base+"/") {
		return fmt.Errorf("refusing to download %s: not on %s", url, c.base)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(resp.Body, limit+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("download %s: larger than %d bytes", url, limit)
	}
	return nil
}
