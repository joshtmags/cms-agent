// Package install puts verified bundles in place on the PC and rolls them back.
//
// Layout under the root (default /opt/cms-arena-agent):
//
//	releases/<version>/     one extracted bundle per version
//	current  -> releases/<version>   what systemd runs
//	previous -> releases/<version>   what a rollback returns to
//	state/pending.json      set right after an update, cleared once the new agent reports healthy
//	state/failed.json       versions that were rolled back; never retried automatically
//	tmp/                    staging for extraction
//
// Rollback is driven from the previous binary (systemd OnFailure and a
// health-check timer run "previous/cms-arena-agent rollback|healthcheck"), so
// a new agent that can't even start is still undone.
package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"github.com/joshtmags/cms-agent/internal/bundle"
)

// DefaultRoot is where the agent lives on a display PC.
const DefaultRoot = "/opt/cms-arena-agent"

// Provisioner applies a release's provision.sh (or a fake, in tests).
type Provisioner func(ctx context.Context, releaseDir string) error

// Pending records an update that hasn't proven itself healthy yet.
type Pending struct {
	Version  string    `json:"version"`
	Previous string    `json:"previous"`
	Deadline time.Time `json:"deadline"`
}

// Layout is the on-disk install of the agent.
type Layout struct {
	Root string
}

func (l Layout) path(parts ...string) string {
	return filepath.Join(append([]string{l.Root}, parts...)...)
}

// CurrentVersion returns the version "current" points to, or "" if none.
func (l Layout) CurrentVersion() (string, error) {
	return l.linkVersion("current")
}

// PreviousVersion returns the version "previous" points to, or "" if none.
func (l Layout) PreviousVersion() (string, error) {
	return l.linkVersion("previous")
}

func (l Layout) linkVersion(name string) (string, error) {
	target, err := os.Readlink(l.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return filepath.Base(target), nil
}

// Install extracts a verified release, runs its provision.sh, and switches
// "current" to it. If provisioning fails nothing is switched. The caller
// restarts the agent afterwards so the new binary takes over.
func (l Layout) Install(ctx context.Context, releaseDir string, m *bundle.Manifest, provision Provisioner, healthWindow time.Duration, now time.Time) error {
	for _, dir := range []string{l.path("releases"), l.path("state"), l.path("tmp")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	staged, err := os.MkdirTemp(l.path("tmp"), m.Version+"-")
	if err != nil {
		return err
	}
	os.Remove(staged) // Extract wants to create it
	if err := bundle.Extract(releaseDir, m, staged); err != nil {
		return err
	}
	final := l.path("releases", m.Version)
	if err := os.RemoveAll(final); err != nil {
		return err
	}
	if err := os.Rename(staged, final); err != nil {
		os.RemoveAll(staged)
		return err
	}
	if err := provision(ctx, final); err != nil {
		os.RemoveAll(final)
		return fmt.Errorf("provision.sh of %s failed, staying on the current version: %w", m.Version, err)
	}

	prev, err := l.CurrentVersion()
	if err != nil {
		return err
	}
	if prev != "" {
		if err := l.setLink("previous", prev); err != nil {
			return err
		}
	}
	if err := l.setLink("current", m.Version); err != nil {
		return err
	}
	return l.writeJSON("pending.json", Pending{Version: m.Version, Previous: prev, Deadline: now.Add(healthWindow)})
}

// MarkHealthy clears the pending update if it is the running version, and
// removes releases other than current and previous.
func (l Layout) MarkHealthy(running string) error {
	p, err := l.pending()
	if err != nil || p == nil || p.Version != running {
		return err
	}
	if err := os.Remove(l.path("state", "pending.json")); err != nil {
		return err
	}
	return l.prune()
}

// CheckHealth rolls back when a pending update missed its deadline.
func (l Layout) CheckHealth(ctx context.Context, provision Provisioner, now time.Time) (bool, error) {
	p, err := l.pending()
	if err != nil || p == nil || now.Before(p.Deadline) {
		return false, err
	}
	return true, l.Rollback(ctx, provision, "not healthy before the deadline")
}

// Rollback returns "current" to the previous release, re-runs that release's
// provision.sh, and records the failed version so it isn't retried. It only
// acts on a pending update: a version that already proved itself healthy is
// never rolled back automatically.
func (l Layout) Rollback(ctx context.Context, provision Provisioner, reason string) error {
	p, err := l.pending()
	if err != nil {
		return err
	}
	if p == nil {
		return errors.New("no update is pending, nothing to roll back")
	}
	if p.Previous == "" {
		return errors.New("no previous version to roll back to")
	}
	if err := l.setLink("current", p.Previous); err != nil {
		return err
	}
	if err := provision(ctx, l.path("releases", p.Previous)); err != nil {
		return fmt.Errorf("rolled back to %s but its provision.sh failed: %w", p.Previous, err)
	}
	if err := l.addFailed(p.Version, reason); err != nil {
		return err
	}
	return os.Remove(l.path("state", "pending.json"))
}

// FailedVersions lists versions that were rolled back.
func (l Layout) FailedVersions() (map[string]string, error) {
	failed := map[string]string{}
	data, err := os.ReadFile(l.path("state", "failed.json"))
	if errors.Is(err, os.ErrNotExist) {
		return failed, nil
	}
	if err != nil {
		return nil, err
	}
	return failed, json.Unmarshal(data, &failed)
}

// Pending returns the pending update, or nil.
func (l Layout) Pending() (*Pending, error) {
	return l.pending()
}

func (l Layout) pending() (*Pending, error) {
	data, err := os.ReadFile(l.path("state", "pending.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Pending
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (l Layout) addFailed(version, reason string) error {
	failed, err := l.FailedVersions()
	if err != nil {
		return err
	}
	failed[version] = reason
	return l.writeJSON("failed.json", failed)
}

func (l Layout) prune() error {
	keep := []string{}
	for _, name := range []string{"current", "previous"} {
		v, err := l.linkVersion(name)
		if err != nil {
			return err
		}
		keep = append(keep, v)
	}
	entries, err := os.ReadDir(l.path("releases"))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !slices.Contains(keep, e.Name()) {
			if err := os.RemoveAll(l.path("releases", e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// setLink atomically points name at releases/<version>.
func (l Layout) setLink(name, version string) error {
	tmp := l.path(name + ".new")
	os.Remove(tmp)
	if err := os.Symlink(filepath.Join("releases", version), tmp); err != nil {
		return err
	}
	return os.Rename(tmp, l.path(name))
}

func (l Layout) writeJSON(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.path("state", name+".new")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.path("state", name))
}

// RunProvisionScript is the real Provisioner: it runs <releaseDir>/provision.sh
// as the agent's user (root), with a time limit.
func RunProvisionScript(ctx context.Context, releaseDir string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(releaseDir, "provision.sh"))
	cmd.Dir = releaseDir
	cmd.Env = append(os.Environ(), "CMS_ARENA_RELEASE_DIR="+releaseDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w\n%s", err, tail(out, 4000))
	}
	return nil
}

func tail(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[len(b)-n:]
}
