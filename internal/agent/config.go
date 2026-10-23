package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/joshtmags/cms-agent/internal/install"
)

// DefaultConfigPath is written by cms-arena-setup during enrollment.
const DefaultConfigPath = "/etc/cms-arena-agent/config.json"

// Config is the agent's local configuration.
type Config struct {
	// CMSURL is the mTLS display host, e.g. https://display.example.com.
	CMSURL string `json:"cms_url"`
	// CertFile and KeyFile are the device certificate and key. (Phase 0 moves
	// the key into the TPM; until then it is a root-only file.)
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`

	PollIntervalSeconds int    `json:"poll_interval_seconds,omitempty"`
	HealthWindowSeconds int    `json:"health_window_seconds,omitempty"`
	Root                string `json:"root,omitempty"`
}

// LoadConfig reads and validates the config file, filling in defaults.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.PollIntervalSeconds == 0 {
		c.PollIntervalSeconds = 15
	}
	if c.HealthWindowSeconds == 0 {
		c.HealthWindowSeconds = 600
	}
	if c.Root == "" {
		c.Root = install.DefaultRoot
	}
	u, err := url.Parse(c.CMSURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("%s: cms_url must be an https URL", path)
	}
	if c.CertFile == "" || c.KeyFile == "" {
		return nil, errors.New(path + ": cert_file and key_file are required")
	}
	return &c, nil
}

func (c *Config) pollInterval() time.Duration {
	return time.Duration(c.PollIntervalSeconds) * time.Second
}

func (c *Config) healthWindow() time.Duration {
	return time.Duration(c.HealthWindowSeconds) * time.Second
}
