// Package agent is the long-running loop on a display PC: poll the CMS,
// report status, and pull signed updates.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/joshtmags/cms-agent/internal/bundle"
	"github.com/joshtmags/cms-agent/internal/install"
	"github.com/joshtmags/cms-agent/internal/minisign"
	"github.com/joshtmags/cms-agent/internal/version"
)

// ErrRestart means a new version was installed: the process should exit so
// systemd starts the new binary.
var ErrRestart = errors.New("new version installed, restarting")

// retryAfter is how long a version that was rejected or failed to install is
// left alone before trying again, so a bad release isn't re-downloaded on
// every poll.
const retryAfter = 10 * time.Minute

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
	a := &Agent{
		cfg:       cfg,
		client:    client,
		layout:    install.Layout{Root: cfg.Root},
		updateKey: updateKey,
		started:   time.Now(),
	}
	a.last = a.loadLastUpdate()
	return a, nil
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
	pending, _ := a.layout.Pending()
	if err := a.layout.MarkHealthy(version.Version); err != nil {
		log.Printf("mark healthy: %v", err)
	} else if pending != nil && pending.Version == version.Version {
		a.record(version.Version, "healthy", nil)
	}
	for _, cmd := range st.Commands {
		a.runCommand(ctx, cmd)
	}
	updateErr := a.maybeUpdate(ctx, st.Agent)
	if err := a.client.SendHeartbeat(ctx, a.heartbeat()); err != nil {
		log.Printf("heartbeat: %v", err)
	}
	return updateErr
}

// runCommand does what the admin asked and reports it. Reboot and shutdown report first:
// afterwards there is no agent left to report anything.
func (a *Agent) runCommand(ctx context.Context, cmd Command) {
	argv := a.cfg.command(cmd.Type)
	if len(argv) == 0 {
		a.report(ctx, cmd, fmt.Errorf("unknown command %q", cmd.Type))
		return
	}
	log.Printf("command %d: %s", cmd.ID, cmd.Type)

	switch cmd.Type {
	case "reboot", "shutdown":
		a.report(ctx, cmd, nil)
		if err := a.exec(ctx, argv); err != nil {
			a.report(ctx, cmd, err)
		}
	case "screenshot":
		dir, err := os.MkdirTemp("", "cms-arena-screenshot-")
		if err != nil {
			a.report(ctx, cmd, err)
			return
		}
		defer os.RemoveAll(dir)
		png := filepath.Join(dir, "screen.png")
		if err := a.exec(ctx, withFile(argv, png)); err != nil {
			a.report(ctx, cmd, err)
			return
		}
		if err := a.client.UploadScreenshot(ctx, cmd.ID, png); err != nil {
			a.report(ctx, cmd, fmt.Errorf("upload: %w", err))
		}
	default:
		a.report(ctx, cmd, a.exec(ctx, argv))
	}
}

func (a *Agent) report(ctx context.Context, cmd Command, err error) {
	errText := ""
	if err != nil {
		errText = err.Error()
		log.Printf("command %d %s failed: %v", cmd.ID, cmd.Type, err)
	}
	if rerr := a.client.ReportCommand(ctx, cmd.ID, err == nil, errText); rerr != nil {
		log.Printf("command %d report: %v", cmd.ID, rerr)
	}
}

func (a *Agent) exec(ctx context.Context, argv []string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func withFile(argv []string, file string) []string {
	out := make([]string, len(argv))
	for i, arg := range argv {
		out[i] = strings.ReplaceAll(arg, "{file}", file)
	}
	return out
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
	// Kept on disk so the result survives the restart that follows an install.
	if data, err := json.Marshal(r); err == nil {
		os.WriteFile(a.lastUpdatePath(), data, 0o644)
	}
}

func (a *Agent) lastUpdatePath() string {
	return filepath.Join(a.cfg.Root, "state", "last-update.json")
}

func (a *Agent) loadLastUpdate() *UpdateResult {
	data, err := os.ReadFile(a.lastUpdatePath())
	if err != nil {
		return nil
	}
	var r UpdateResult
	if json.Unmarshal(data, &r) != nil {
		return nil
	}
	return &r
}

func (a *Agent) maybeUpdate(ctx context.Context, want AgentState) error {
	v := want.DesiredVersion
	if v == "" || v == version.Version {
		return nil
	}
	if failed, err := a.layout.FailedVersions(); err == nil {
		if reason, ok := failed[v]; ok {
			// A rolled-back version is never retried automatically; publish a newer one.
			if a.last == nil || a.last.Version != v || a.last.Status != "rolled_back" {
				a.record(v, "rolled_back", errors.New(reason))
			}
			return nil
		}
	}
	if a.last != nil && a.last.Version == v && (a.last.Status == "rejected" || a.last.Status == "failed") &&
		time.Since(a.last.At) < retryAfter {
		return nil
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
