package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// State is the CMS's answer to GET /device-api/v1/state (the part the agent
// uses so far; display layout goes to the page, not the agent).
type State struct {
	Agent AgentState `json:"agent"`
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
	Status  string    `json:"status"` // waiting, rejected, failed, installed
	Error   string    `json:"error,omitempty"`
	At      time.Time `json:"at"`
}

// Client talks to the display host over mTLS.
type Client struct {
	base string
	http *http.Client
}

// NewClient loads the device certificate and builds an mTLS client. The
// server is verified against the system roots (the display host has a public
// certificate).
func NewClient(c *Config) (*Client, error) {
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("device certificate: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	return &Client{
		base: strings.TrimRight(c.CMSURL, "/"),
		http: &http.Client{Transport: transport, Timeout: 30 * time.Second},
	}, nil
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
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("state: HTTP %d", resp.StatusCode)
	}
	var st State
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil {
		return nil, fmt.Errorf("state: %w", err)
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
		return fmt.Errorf("heartbeat: HTTP %d", resp.StatusCode)
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
