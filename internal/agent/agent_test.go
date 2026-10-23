package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joshtmags/cms-agent/internal/bundle"
	"github.com/joshtmags/cms-agent/internal/install"
	"github.com/joshtmags/cms-agent/internal/minisign"
	"github.com/joshtmags/cms-agent/internal/minisign/minisigntest"
	"github.com/joshtmags/cms-agent/internal/version"
)

// fakeCMS serves /device-api/v1/state, the heartbeat endpoint and one release.
type fakeCMS struct {
	srv        *httptest.Server
	state      AgentState
	releaseDir string
	heartbeats []Heartbeat
}

func newFakeCMS(t *testing.T, releaseDir string) *fakeCMS {
	f := &fakeCMS{releaseDir: releaseDir}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /device-api/v1/state", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(State{Agent: f.state})
	})
	mux.HandleFunc("POST /device-api/v1/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var hb Heartbeat
		json.NewDecoder(r.Body).Decode(&hb)
		f.heartbeats = append(f.heartbeats, hb)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /device-api/v1/bundles/{version}/{file}", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(f.releaseDir, r.PathValue("file")))
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func signedRelease(t *testing.T, s minisigntest.Signer, v string) string {
	t.Helper()
	stage := t.TempDir()
	os.WriteFile(filepath.Join(stage, bundle.Name), []byte("new binary"), 0o755)
	os.WriteFile(filepath.Join(stage, "provision.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	out := t.TempDir()
	if _, err := bundle.Build(stage, out, v, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(out, bundle.ManifestFile))
	os.WriteFile(filepath.Join(out, bundle.SignatureFile), s.Sign(data, "cms-arena-agent "+v), 0o644)
	return out
}

func testAgent(t *testing.T, f *fakeCMS, s minisigntest.Signer) *Agent {
	t.Helper()
	pk, err := minisign.ParsePublicKey(s.PublicKeyFile())
	if err != nil {
		t.Fatal(err)
	}
	old := version.Version
	version.Version = "1.0.0"
	t.Cleanup(func() { version.Version = old })
	cfg := &Config{CMSURL: f.srv.URL, PollIntervalSeconds: 1, HealthWindowSeconds: 600, Root: t.TempDir()}
	return &Agent{
		cfg:       cfg,
		client:    &Client{base: f.srv.URL, http: f.srv.Client()},
		layout:    install.Layout{Root: cfg.Root},
		updateKey: pk,
		started:   time.Now(),
	}
}

func (f *fakeCMS) want(v string, safe bool) {
	f.state = AgentState{DesiredVersion: v, BundleURL: f.srv.URL + "/device-api/v1/bundles/" + v, SafeToUpdate: safe}
}

func TestSignedUpdateIsInstalled(t *testing.T) {
	s := minisigntest.New(t)
	f := newFakeCMS(t, signedRelease(t, s, "1.1.0"))
	a := testAgent(t, f, s)
	f.want("1.1.0", true)

	if err := a.tick(context.Background()); !errors.Is(err, ErrRestart) {
		t.Fatalf("tick = %v, want ErrRestart", err)
	}
	if cur, _ := a.layout.CurrentVersion(); cur != "1.1.0" {
		t.Fatalf("current = %q", cur)
	}
	if hb := f.heartbeats[len(f.heartbeats)-1]; hb.LastUpdate == nil || hb.LastUpdate.Status != "installed" {
		t.Fatalf("heartbeat last_update = %+v", hb.LastUpdate)
	}
}

func TestUpdateWaitsUntilSafe(t *testing.T) {
	s := minisigntest.New(t)
	f := newFakeCMS(t, signedRelease(t, s, "1.1.0"))
	a := testAgent(t, f, s)
	f.want("1.1.0", false)

	if err := a.tick(context.Background()); err != nil {
		t.Fatalf("tick = %v", err)
	}
	if cur, _ := a.layout.CurrentVersion(); cur != "" {
		t.Fatal("installed during a fight")
	}
	if a.last == nil || a.last.Status != "waiting" {
		t.Fatalf("last = %+v, want waiting", a.last)
	}
}

func TestUpdateSignedByOtherKeyIsRejected(t *testing.T) {
	trusted, attacker := minisigntest.New(t), minisigntest.New(t)
	f := newFakeCMS(t, signedRelease(t, attacker, "1.1.0"))
	a := testAgent(t, f, trusted)
	f.want("1.1.0", true)

	if err := a.tick(context.Background()); err != nil {
		t.Fatalf("tick = %v", err)
	}
	if cur, _ := a.layout.CurrentVersion(); cur != "" {
		t.Fatal("installed a bundle signed by another key")
	}
	if a.last == nil || a.last.Status != "rejected" || !strings.Contains(a.last.Error, "signature") {
		t.Fatalf("last = %+v, want a signature rejection", a.last)
	}
}

func TestUpdateWithSwappedTarballIsRejected(t *testing.T) {
	s := minisigntest.New(t)
	dir := signedRelease(t, s, "1.1.0")
	tarball := filepath.Join(dir, bundle.TarballName("1.1.0"))
	data, _ := os.ReadFile(tarball)
	data[len(data)/2] ^= 0xff
	os.WriteFile(tarball, data, 0o644)

	f := newFakeCMS(t, dir)
	a := testAgent(t, f, s)
	f.want("1.1.0", true)
	a.tick(context.Background())
	if cur, _ := a.layout.CurrentVersion(); cur != "" {
		t.Fatal("installed a tampered tarball")
	}
	if a.last == nil || a.last.Status != "rejected" {
		t.Fatalf("last = %+v", a.last)
	}
}

func TestCMSCannotDowngrade(t *testing.T) {
	s := minisigntest.New(t)
	f := newFakeCMS(t, signedRelease(t, s, "0.9.0"))
	a := testAgent(t, f, s)
	f.want("0.9.0", true)
	a.tick(context.Background())
	if cur, _ := a.layout.CurrentVersion(); cur != "" {
		t.Fatal("downgraded without a signed rollback release")
	}
}
