// Package agent is the long-running loop on a display PC: poll the CMS,
// report status, and pull signed updates.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/joshtmags/cms-agent/internal/bundle"
	"github.com/joshtmags/cms-agent/internal/install"
	"github.com/joshtmags/cms-agent/internal/minisign"
	"github.com/joshtmags/cms-agent/internal/version"
)

// ErrRestart means a new version was installed: the process should exit so
// systemd starts the new binary.
var ErrRestart = errors.New("new version installed, restarting")

// Agent runs the poll loop.
type Agent struct {
	cfg       *Config
	client    *Client
	layout    install.Layout
	updateKey minisign.PublicKey
	started   time.Time
	last      *UpdateResult
}

// New builds an agent. updateKey is the embedded signing key.
func New(cfg *Config, updateKey minisign.PublicKey) (*Agent, error) {
	client, err := NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Agent{
		cfg:       cfg,
		client:    client,
		layout:    install.Layout{Root: cfg.Root},
		updateKey: updateKey,
		started:   time.Now(),
	}, nil
}

// Run polls until ctx is cancelled or a new version is installed.
func (a *Agent) Run(ctx context.Context) error {
	log.Printf("cms-arena-agent %s polling %s every %s", version.Version, a.cfg.CMSURL, a.cfg.pollInterval())
	for {
		if err := a.tick(ctx); errors.Is(err, ErrRestart) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(a.cfg.pollInterval()):
		}
	}
}

func (a *Agent) tick(ctx context.Context) error {
	st, err := a.client.State(ctx)
	if err != nil {
		log.Printf("state: %v", err)
		return nil
	}
	// Reaching the CMS with our certificate is the proof that a fresh update works.
	if err := a.layout.MarkHealthy(version.Version); err != nil {
		log.Printf("mark healthy: %v", err)
	}
	updateErr := a.maybeUpdate(ctx, st.Agent)
	if err := a.client.SendHeartbeat(ctx, a.heartbeat()); err != nil {
		log.Printf("heartbeat: %v", err)
	}
	return updateErr
}

func (a *Agent) heartbeat() Heartbeat {
	host, _ := os.Hostname()
	hb := Heartbeat{
		AgentVersion:  version.Version,
		Hostname:      host,
		UptimeSeconds: int64(time.Since(a.started).Seconds()),
		LastUpdate:    a.last,
	}
	if p, err := a.layout.Pending(); err == nil && p != nil {
		hb.PendingUpdate = p.Version
	}
	if failed, err := a.layout.FailedVersions(); err == nil && len(failed) > 0 {
		hb.FailedVersions = failed
	}
	return hb
}

func (a *Agent) record(v, status string, err error) {
	r := &UpdateResult{Version: v, Status: status, At: time.Now().UTC()}
	if err != nil {
		r.Error = err.Error()
		log.Printf("update %s %s: %v", v, status, err)
	} else {
		log.Printf("update %s %s", v, status)
	}
	a.last = r
}

func (a *Agent) maybeUpdate(ctx context.Context, want AgentState) error {
	v := want.DesiredVersion
	if v == "" || v == version.Version {
		return nil
	}
	if failed, err := a.layout.FailedVersions(); err == nil {
		if reason, ok := failed[v]; ok {
			// A rolled-back version is never retried automatically; publish a newer one.
			if a.last == nil || a.last.Version != v {
				a.record(v, "rejected", fmt.Errorf("was rolled back before: %s", reason))
			}
			return nil
		}
	}
	if !want.SafeToUpdate {
		if a.last == nil || a.last.Version != v || a.last.Status != "waiting" {
			a.record(v, "waiting", nil)
		}
		return nil
	}

	dir, err := os.MkdirTemp("", "cms-arena-update-")
	if err != nil {
		a.record(v, "failed", err)
		return nil
	}
	defer os.RemoveAll(dir)

	m, err := a.download(ctx, want, dir)
	if err != nil {
		a.record(v, "rejected", err)
		return nil
	}
	if err := a.layout.Install(ctx, dir, m, install.RunProvisionScript, a.cfg.healthWindow(), time.Now()); err != nil {
		a.record(v, "failed", err)
		return nil
	}
	a.record(v, "installed", nil)
	return ErrRestart
}

// download fetches and verifies a release: the small signed manifest first,
// the tarball only once the manifest's signature and version check out.
func (a *Agent) download(ctx context.Context, want AgentState, dir string) (*bundle.Manifest, error) {
	for _, name := range []string{bundle.ManifestFile, bundle.SignatureFile} {
		if err := a.client.Download(ctx, want.BundleURL+"/"+name, filepath.Join(dir, name), 1<<20); err != nil {
			return nil, err
		}
	}
	m, err := bundle.VerifyManifestFiles(dir, a.updateKey)
	if err != nil {
		return nil, err
	}
	if m.Version != want.DesiredVersion {
		return nil, fmt.Errorf("CMS asked for %s but the signed manifest is %s", want.DesiredVersion, m.Version)
	}
	if err := bundle.CheckUpgrade(version.Version, m); err != nil {
		return nil, err
	}
	if err := a.client.Download(ctx, want.BundleURL+"/"+m.Tarball.Path, filepath.Join(dir, m.Tarball.Path), m.Tarball.Size); err != nil {
		return nil, err
	}
	if err := bundle.CheckTarball(dir, m); err != nil {
		return nil, err
	}
	return m, nil
}
