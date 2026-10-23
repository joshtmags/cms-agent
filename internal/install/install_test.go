package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/joshtmags/cms-agent/internal/bundle"
)

var t0 = time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)

// release builds an (unsigned) release dir; signature checks live in the
// bundle package, Install only needs the manifest and tarball.
func release(t *testing.T, version string) (string, *bundle.Manifest) {
	t.Helper()
	stage := t.TempDir()
	os.WriteFile(filepath.Join(stage, bundle.Name), []byte("bin "+version), 0o755)
	os.WriteFile(filepath.Join(stage, "provision.sh"), []byte("#!/bin/sh\n"), 0o755)
	out := t.TempDir()
	m, err := bundle.Build(stage, out, version, t0, false)
	if err != nil {
		t.Fatal(err)
	}
	return out, m
}

type provisionLog struct{ ran []string }

func (p *provisionLog) ok(_ context.Context, dir string) error {
	p.ran = append(p.ran, filepath.Base(dir))
	return nil
}

func failing(context.Context, string) error { return errors.New("boom") }

func install(t *testing.T, l Layout, version string, prov Provisioner) error {
	t.Helper()
	dir, m := release(t, version)
	return l.Install(context.Background(), dir, m, prov, 5*time.Minute, t0)
}

func mustVersion(t *testing.T, got string, err error, want string) {
	t.Helper()
	if err != nil || got != want {
		t.Fatalf("version = %q, %v; want %q", got, err, want)
	}
}

func TestInstallSwitchesCurrentAndKeepsPrevious(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	log := &provisionLog{}
	if err := install(t, l, "1.0.0", log.ok); err != nil {
		t.Fatal(err)
	}
	l.MarkHealthy("1.0.0")
	if err := install(t, l, "1.1.0", log.ok); err != nil {
		t.Fatal(err)
	}
	cur, err := l.CurrentVersion()
	mustVersion(t, cur, err, "1.1.0")
	prev, err := l.PreviousVersion()
	mustVersion(t, prev, err, "1.0.0")
	if p, _ := l.Pending(); p == nil || p.Version != "1.1.0" || p.Previous != "1.0.0" {
		t.Fatalf("pending = %+v", p)
	}
	bin, _ := os.ReadFile(filepath.Join(l.Root, "current", bundle.Name))
	if string(bin) != "bin 1.1.0" {
		t.Fatalf("current binary = %q", bin)
	}
}

func TestInstallFailedProvisionDoesNotSwitch(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	log := &provisionLog{}
	install(t, l, "1.0.0", log.ok)
	l.MarkHealthy("1.0.0")
	if err := install(t, l, "1.1.0", failing); err == nil {
		t.Fatal("expected provision failure")
	}
	cur, err := l.CurrentVersion()
	mustVersion(t, cur, err, "1.0.0")
	if _, err := os.Stat(filepath.Join(l.Root, "releases", "1.1.0")); !os.IsNotExist(err) {
		t.Fatal("failed release left on disk")
	}
}

func TestHealthyUpdateIsKeptAndOldReleasesPruned(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	log := &provisionLog{}
	for _, v := range []string{"1.0.0", "1.1.0", "1.2.0"} {
		if err := install(t, l, v, log.ok); err != nil {
			t.Fatal(err)
		}
		if err := l.MarkHealthy(v); err != nil {
			t.Fatal(err)
		}
	}
	if rolled, err := l.CheckHealth(context.Background(), log.ok, t0.Add(time.Hour)); rolled || err != nil {
		t.Fatalf("healthy update rolled back: %v %v", rolled, err)
	}
	entries, _ := os.ReadDir(filepath.Join(l.Root, "releases"))
	if len(entries) != 2 {
		t.Fatalf("releases kept = %d, want 2 (current + previous)", len(entries))
	}
}

func TestMissedDeadlineRollsBack(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	log := &provisionLog{}
	install(t, l, "1.0.0", log.ok)
	l.MarkHealthy("1.0.0")
	install(t, l, "1.1.0", log.ok)

	if rolled, _ := l.CheckHealth(context.Background(), log.ok, t0.Add(time.Minute)); rolled {
		t.Fatal("rolled back before the deadline")
	}
	rolled, err := l.CheckHealth(context.Background(), log.ok, t0.Add(10*time.Minute))
	if !rolled || err != nil {
		t.Fatalf("CheckHealth = %v, %v; want rollback", rolled, err)
	}
	cur, err := l.CurrentVersion()
	mustVersion(t, cur, err, "1.0.0")
	if got := log.ran[len(log.ran)-1]; got != "1.0.0" {
		t.Fatalf("last provision ran for %s, want the previous release 1.0.0", got)
	}
	failed, _ := l.FailedVersions()
	if _, ok := failed["1.1.0"]; !ok {
		t.Fatal("rolled-back version not recorded as failed")
	}
}

func TestRollbackNeedsAPendingUpdate(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	log := &provisionLog{}
	install(t, l, "1.0.0", log.ok)
	l.MarkHealthy("1.0.0")
	install(t, l, "1.1.0", log.ok)
	l.MarkHealthy("1.1.0")
	if err := l.Rollback(context.Background(), log.ok, "crash loop"); err == nil {
		t.Fatal("rolled back a version that had already proved healthy")
	}
}
