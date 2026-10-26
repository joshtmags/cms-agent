// Command cms-arena-agent runs on each arena display PC.
//
//	cms-arena-agent enroll --server URL --code XXXX-XXXX   get this PC's certificate (once, at setup; run again to resume)
//	cms-arena-agent run                      poll the CMS, report status, pull signed updates (systemd)
//	cms-arena-agent version                  print the version
//	cms-arena-agent verify --dir DIR         verify a downloaded release exactly as a PC would
//	cms-arena-agent apply --dir DIR          verify and install a release by hand (over Tailscale SSH)
//	cms-arena-agent healthcheck              roll back an update that missed its health deadline (systemd timer)
//	cms-arena-agent rollback --reason TEXT   roll back the pending update now (systemd OnFailure)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/joshtmags/cms-agent/internal/agent"
	"github.com/joshtmags/cms-agent/internal/bundle"
	"github.com/joshtmags/cms-agent/internal/enroll"
	"github.com/joshtmags/cms-agent/internal/install"
	"github.com/joshtmags/cms-agent/internal/minisign"
	"github.com/joshtmags/cms-agent/internal/version"
	"github.com/joshtmags/cms-agent/keys"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "enroll":
		err = enrollCmd(ctx, args)
	case "run":
		err = run(ctx, args)
	case "version":
		fmt.Println(version.Version)
	case "verify":
		err = verify(args)
	case "apply":
		err = apply(ctx, args)
	case "healthcheck":
		err = healthcheck(ctx, args)
	case "rollback":
		err = rollback(ctx, args)
	default:
		usage()
	}
	if err != nil {
		log.Fatalf("cms-arena-agent %s: %v", os.Args[1], err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: cms-arena-agent enroll|run|version|verify|apply|healthcheck|rollback [flags]")
	os.Exit(2)
}

func enrollCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	server := fs.String("server", "", "the CMS, e.g. https://cms.example.com (the normal host, not the display host)")
	code := fs.String("code", "", "one-time enrollment code from the CMS (not needed to resume an enrollment in progress)")
	dir := fs.String("dir", filepath.Dir(agent.DefaultConfigPath), "where the key, certificate and config go")
	root := fs.String("root", install.DefaultRoot, "install root written into config.json")
	force := fs.Bool("force", false, "enroll again although this PC is enrolled (the old certificate stays until the new one is approved)")
	fs.Parse(args)
	if *server == "" {
		return errors.New("--server is required")
	}
	if err := enroll.Run(ctx, enroll.Options{Server: *server, Code: *code, Dir: *dir, Root: *root, Force: *force, Out: os.Stdout}); err != nil {
		return err
	}
	// The kiosk browser needs the new certificate too (installed by provision.sh on a display PC).
	if _, err := os.Stat(kioskCertHelper); err == nil {
		cmd := exec.Command(kioskCertHelper)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("enrolled, but %s failed: %w (run it again)", kioskCertHelper, err)
		}
	}
	return nil
}

const kioskCertHelper = "/usr/local/bin/cms-arena-kiosk-cert"

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", agent.DefaultConfigPath, "config file")
	fs.Parse(args)

	cfg, err := agent.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	key, err := keys.UpdateKey()
	if err != nil {
		return fmt.Errorf("no valid update-signing key built in: %w", err)
	}
	if keys.UsingLocalTestKey() {
		log.Print("WARNING: this build trusts a LOCAL TEST signing key")
	}
	a, err := agent.New(cfg, key)
	if err != nil {
		return err
	}
	err = a.Run(ctx)
	if errors.Is(err, agent.ErrRestart) || errors.Is(err, context.Canceled) {
		// Exit cleanly; systemd (Restart=always) starts current/cms-arena-agent again.
		return nil
	}
	return err
}

// trustedKey is the embedded key, or a key file for trying out verification
// on a dev machine before the real key is built in.
func trustedKey(pubkeyPath string) (minisign.PublicKey, error) {
	if pubkeyPath == "" {
		return keys.UpdateKey()
	}
	data, err := os.ReadFile(pubkeyPath)
	if err != nil {
		return minisign.PublicKey{}, err
	}
	return minisign.ParsePublicKey(data)
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dir := fs.String("dir", ".", "directory with manifest.json, manifest.json.minisig and the tarball")
	pubkey := fs.String("pubkey", "", "minisign public key file (default: the key built into this binary)")
	fs.Parse(args)

	key, err := trustedKey(*pubkey)
	if err != nil {
		return err
	}
	m, err := bundle.VerifyRelease(*dir, key)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "cms-arena-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := bundle.Extract(*dir, m, tmp+"/x"); err != nil {
		return err
	}
	fmt.Printf("OK: signature valid, cms-arena-agent %s, built %s, %d files match the manifest\n",
		m.Version, m.CreatedAt.Format(time.RFC3339), len(m.Files))
	for _, f := range m.Files {
		fmt.Printf("  %s  %s  %8d  %s\n", f.Mode, f.SHA256[:12], f.Size, f.Path)
	}
	return nil
}

func apply(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	dir := fs.String("dir", ".", "downloaded release directory")
	root := fs.String("root", install.DefaultRoot, "install root")
	fs.Parse(args)

	key, err := keys.UpdateKey()
	if err != nil {
		return err
	}
	m, err := bundle.VerifyRelease(*dir, key)
	if err != nil {
		return err
	}
	l := install.Layout{Root: *root}
	running, err := l.CurrentVersion()
	if err != nil {
		return err
	}
	if running != "" {
		if err := bundle.CheckUpgrade(running, m); err != nil {
			return err
		}
	}
	if err := l.Install(ctx, *dir, m, install.RunProvisionScript, 10*time.Minute, time.Now()); err != nil {
		return err
	}
	fmt.Printf("installed %s; restarting the agent\n", m.Version)
	return restartAgent()
}

func healthcheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	root := fs.String("root", install.DefaultRoot, "install root")
	fs.Parse(args)

	l := install.Layout{Root: *root}
	rolled, err := l.CheckHealth(ctx, install.RunProvisionScript, time.Now())
	if err != nil {
		return err
	}
	if rolled {
		log.Print("update missed its health deadline; rolled back")
		return restartAgent()
	}
	return nil
}

func rollback(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ExitOnError)
	root := fs.String("root", install.DefaultRoot, "install root")
	reason := fs.String("reason", "manual rollback", "recorded in state/failed.json")
	fs.Parse(args)

	l := install.Layout{Root: *root}
	if err := l.Rollback(ctx, install.RunProvisionScript, *reason); err != nil {
		return err
	}
	log.Printf("rolled back: %s", *reason)
	return nil
}

func restartAgent() error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		log.Print("no systemd here; restart the agent yourself")
		return nil
	}
	return exec.Command("systemctl", "restart", "--no-block", "cms-arena-agent.service").Run()
}
