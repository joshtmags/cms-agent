package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	srv         *httptest.Server
	state       AgentState
	commands    []Command
	reports     []map[string]any
	screenshots map[string][]byte
	releaseDir  string
	heartbeats  []Heartbeat
	downloads   int
}

func newFakeCMS(t *testing.T, releaseDir string) *fakeCMS {
	f := &fakeCMS{releaseDir: releaseDir, screenshots: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /device-api/v1/state", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(State{Agent: f.state, Commands: f.commands})
		f.commands = nil // handed out once
	})
	mux.HandleFunc("POST /device-api/v1/commands/{id}/result", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		body["id"] = r.PathValue("id")
		f.reports = append(f.reports, body)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /device-api/v1/commands/{id}/screenshot", func(w http.ResponseWriter, r *http.Request) {
		file, _, err := r.FormFile("screenshot")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		data, _ := io.ReadAll(file)
		f.screenshots[r.PathValue("id")] = data
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /device-api/v1/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var hb Heartbeat
		json.NewDecoder(r.Body).Decode(&hb)
		f.heartbeats = append(f.heartbeats, hb)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /device-api/v1/bundles/{version}/{file}", func(w http.ResponseWriter, r *http.Request) {
		f.downloads++
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

func TestNewProcessReportsTheUpdateAsHealthy(t *testing.T) {
	s := minisigntest.New(t)
	f := newFakeCMS(t, signedRelease(t, s, "1.1.0"))
	a := testAgent(t, f, s)
	f.want("1.1.0", true)
	if err := a.tick(context.Background()); !errors.Is(err, ErrRestart) {
		t.Fatalf("tick = %v", err)
	}

	// The restarted process: new version, fresh memory, same disk.
	version.Version = "1.1.0"
	restarted := &Agent{cfg: a.cfg, client: a.client, layout: a.layout, updateKey: a.updateKey, started: time.Now()}
	restarted.last = restarted.loadLastUpdate()
	if restarted.last == nil || restarted.last.Status != "installed" {
		t.Fatalf("last update after restart = %+v, want installed", restarted.last)
	}
	if err := restarted.tick(context.Background()); err != nil {
		t.Fatalf("tick = %v", err)
	}
	if hb := f.heartbeats[len(f.heartbeats)-1]; hb.LastUpdate == nil || hb.LastUpdate.Status != "healthy" {
		t.Fatalf("heartbeat last_update = %+v, want healthy", hb.LastUpdate)
	}
	if p, _ := restarted.layout.Pending(); p != nil {
		t.Fatal("pending update not cleared")
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

func TestRejectedVersionIsNotRetriedOnEveryPoll(t *testing.T) {
	trusted, attacker := minisigntest.New(t), minisigntest.New(t)
	f := newFakeCMS(t, signedRelease(t, attacker, "1.1.0"))
	a := testAgent(t, f, trusted)
	f.want("1.1.0", true)

	a.tick(context.Background())
	after := f.downloads
	a.tick(context.Background())
	a.tick(context.Background())
	if f.downloads != after {
		t.Fatalf("re-downloaded a rejected release: %d downloads after the first try, want %d", f.downloads, after)
	}
	a.last.At = time.Now().Add(-retryAfter - time.Second)
	a.tick(context.Background())
	if f.downloads == after {
		t.Fatal("never retried after the back-off")
	}
}

func TestRolledBackVersionIsReportedAndNotRetried(t *testing.T) {
	s := minisigntest.New(t)
	f := newFakeCMS(t, signedRelease(t, s, "1.1.0"))
	a := testAgent(t, f, s)
	os.MkdirAll(filepath.Join(a.cfg.Root, "state"), 0o755)
	os.WriteFile(filepath.Join(a.cfg.Root, "state", "failed.json"), []byte(`{"1.1.0":"not healthy before the deadline"}`), 0o644)
	// What the process remembers from just before the failed install.
	a.last = &UpdateResult{Version: "1.1.0", Status: "installed", At: time.Now()}
	f.want("1.1.0", true)

	a.tick(context.Background())
	if f.downloads != 0 {
		t.Fatal("downloaded a version that was rolled back")
	}
	if hb := f.heartbeats[len(f.heartbeats)-1]; hb.LastUpdate == nil || hb.LastUpdate.Status != "rolled_back" {
		t.Fatalf("heartbeat last_update = %+v, want rolled_back", hb.LastUpdate)
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

func commandAgent(t *testing.T) (*fakeCMS, *Agent) {
	t.Helper()
	s := minisigntest.New(t)
	f := newFakeCMS(t, t.TempDir())
	a := testAgent(t, f, s)
	a.cfg.Commands = map[string][]string{
		"reboot":          {"sh", "-c", "echo cannot reboot from a test >&2; exit 1"},
		"restart_browser": {"sh", "-c", "echo kiosk unit missing >&2; exit 3"},
		"screenshot":      {"sh", "-c", `printf 'PNGDATA' > "$0"`, "{file}"},
		"shutdown":        {"true"},
	}
	return f, a
}

func TestRebootIsReportedBeforeItRuns(t *testing.T) {
	f, a := commandAgent(t)
	f.commands = []Command{{ID: 7, Type: "reboot"}}
	a.tick(context.Background())

	if len(f.reports) != 2 || f.reports[0]["ok"] != true || f.reports[1]["ok"] != false {
		t.Fatalf("reports = %v, want ok first (before the reboot), then the failure", f.reports)
	}
}

func TestFailedCommandReportsItsOutput(t *testing.T) {
	f, a := commandAgent(t)
	f.commands = []Command{{ID: 8, Type: "restart_browser"}, {ID: 9, Type: "fly"}}
	a.tick(context.Background())

	if len(f.reports) != 2 {
		t.Fatalf("reports = %v", f.reports)
	}
	if f.reports[0]["ok"] != false || !strings.Contains(f.reports[0]["error"].(string), "kiosk unit missing") {
		t.Fatalf("restart_browser report = %v", f.reports[0])
	}
	if f.reports[1]["ok"] != false || !strings.Contains(f.reports[1]["error"].(string), "unknown command") {
		t.Fatalf("unknown command report = %v", f.reports[1])
	}
}

func TestScreenshotIsUploaded(t *testing.T) {
	f, a := commandAgent(t)
	f.commands = []Command{{ID: 10, Type: "screenshot"}}
	a.tick(context.Background())

	if string(f.screenshots["10"]) != "PNGDATA" {
		t.Fatalf("screenshot = %q, reports = %v", f.screenshots["10"], f.reports)
	}
}
