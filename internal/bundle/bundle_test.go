package bundle

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joshtmags/cms-agent/internal/minisign"
	"github.com/joshtmags/cms-agent/internal/minisign/minisigntest"
)

func writeStage(t *testing.T) string {
	t.Helper()
	stage := t.TempDir()
	write := func(rel, content string, mode os.FileMode) {
		p := filepath.Join(stage, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(Name, "binary", 0o755)
	write("provision.sh", "#!/bin/sh\necho ok\n", 0o755)
	write("files/systemd/cms-arena-agent.service", "[Unit]\n", 0o644)
	return stage
}

// buildSigned builds a release into a temp dir and signs it.
func buildSigned(t *testing.T, s minisigntest.Signer, version string) (string, *Manifest) {
	t.Helper()
	out := t.TempDir()
	m, err := Build(writeStage(t), out, version, time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC), false)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, SignatureFile), s.Sign(data, "cms-arena-agent "+version), 0o644); err != nil {
		t.Fatal(err)
	}
	return out, m
}

func publicKey(t *testing.T, s minisigntest.Signer) minisign.PublicKey {
	t.Helper()
	pk, err := minisign.ParsePublicKey(s.PublicKeyFile())
	if err != nil {
		t.Fatal(err)
	}
	return pk
}

func TestBuildVerifyExtractRoundTrip(t *testing.T) {
	s := minisigntest.New(t)
	dir, built := buildSigned(t, s, "1.1.0")

	m, err := VerifyRelease(dir, publicKey(t, s))
	if err != nil {
		t.Fatalf("VerifyRelease: %v", err)
	}
	if m.Version != "1.1.0" || len(m.Files) != len(built.Files) {
		t.Fatalf("unexpected manifest %+v", m)
	}
	dest := filepath.Join(t.TempDir(), "out")
	if err := Extract(dir, m, dest); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	info, err := os.Stat(filepath.Join(dest, "provision.sh"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("provision.sh mode = %v, %v", info.Mode(), err)
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	stage := writeStage(t)
	at := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	a, err := Build(stage, t.TempDir(), "1.0.0", at, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Build(stage, t.TempDir(), "1.0.0", at, false)
	if err != nil {
		t.Fatal(err)
	}
	if a.Tarball.SHA256 != b.Tarball.SHA256 {
		t.Fatal("two builds of the same stage produced different tarballs")
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	dir, _ := buildSigned(t, minisigntest.New(t), "1.1.0")
	if _, err := VerifyRelease(dir, publicKey(t, minisigntest.New(t))); err == nil {
		t.Fatal("release signed by another key accepted")
	}
}

func TestVerifyRejectsEditedManifest(t *testing.T) {
	s := minisigntest.New(t)
	dir, _ := buildSigned(t, s, "1.1.0")
	p := filepath.Join(dir, ManifestFile)
	data, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(data), `"1.1.0"`, `"9.0.0"`, 1)), 0o644)
	if _, err := VerifyRelease(dir, publicKey(t, s)); err == nil {
		t.Fatal("edited manifest accepted")
	}
}

func TestVerifyRejectsSwappedTarball(t *testing.T) {
	s := minisigntest.New(t)
	dir, m := buildSigned(t, s, "1.1.0")
	// Replace the tarball with a different, validly built one.
	other := t.TempDir()
	stage := writeStage(t)
	os.WriteFile(filepath.Join(stage, "provision.sh"), []byte("#!/bin/sh\nrm -rf /\n"), 0o755)
	if _, err := Build(stage, other, "1.1.0", time.Now(), false); err != nil {
		t.Fatal(err)
	}
	evil, _ := os.ReadFile(filepath.Join(other, m.Tarball.Path))
	os.WriteFile(filepath.Join(dir, m.Tarball.Path), evil, 0o644)
	if _, err := VerifyRelease(dir, publicKey(t, s)); err == nil {
		t.Fatal("swapped tarball accepted")
	}
}

func TestExtractRejectsUnlistedEntry(t *testing.T) {
	s := minisigntest.New(t)
	dir, m := buildSigned(t, s, "1.1.0")
	// Rewrite the tarball with an extra entry; the hash check is bypassed on
	// purpose to prove Extract is strict on its own.
	p := filepath.Join(dir, m.Tarball.Path)
	f, _ := os.Create(p)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "../evil", Size: 1, Mode: 0o644})
	tw.Write([]byte("x"))
	tw.Close()
	gz.Close()
	f.Close()
	dest := filepath.Join(t.TempDir(), "out")
	if err := Extract(dir, m, dest); err == nil {
		t.Fatal("unlisted tarball entry accepted")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("failed extraction left files behind")
	}
}

func TestParseManifestRejectsBadPaths(t *testing.T) {
	for _, bad := range []string{"../x", "/etc/passwd", "a/../b", "./a", `a\b`} {
		if validPath(bad) {
			t.Errorf("validPath(%q) = true", bad)
		}
	}
}

func TestCheckUpgrade(t *testing.T) {
	cases := []struct {
		running, next string
		rollback, ok  bool
	}{
		{"1.0.0", "1.1.0", false, true},
		{"1.1.0", "1.1.0", false, false},
		{"1.1.0", "1.0.0", false, false},
		{"1.1.0", "1.0.0", true, true},
		{"1.9.0", "1.10.0", false, true},
	}
	for _, c := range cases {
		err := CheckUpgrade(c.running, &Manifest{Version: c.next, Rollback: c.rollback})
		if (err == nil) != c.ok {
			t.Errorf("CheckUpgrade(%s -> %s, rollback=%v) err = %v", c.running, c.next, c.rollback, err)
		}
	}
}
