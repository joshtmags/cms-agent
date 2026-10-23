package bundle

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/joshtmags/cms-agent/internal/minisign"
)

// VerifyRelease checks a downloaded release directory (manifest.json,
// manifest.json.minisig and the tarball) against the trusted public key.
// Nothing from the tarball is read until the signature and the tarball hash
// both check out.
func VerifyRelease(dir string, pk minisign.PublicKey) (*Manifest, error) {
	m, err := VerifyManifestFiles(dir, pk)
	if err != nil {
		return nil, err
	}
	if err := CheckTarball(dir, m); err != nil {
		return nil, err
	}
	return m, nil
}

// VerifyManifestFiles verifies dir/manifest.json against dir/manifest.json.minisig.
// The agent calls this before downloading the (much larger) tarball.
func VerifyManifestFiles(dir string, pk minisign.PublicKey) (*Manifest, error) {
	manifestData, err := readLimited(filepath.Join(dir, ManifestFile), maxManifestBytes)
	if err != nil {
		return nil, err
	}
	sigData, err := readLimited(filepath.Join(dir, SignatureFile), 4096)
	if err != nil {
		return nil, err
	}
	return VerifyManifest(manifestData, sigData, pk)
}

// VerifyManifest checks the signature first and only then parses the manifest.
func VerifyManifest(manifestData, sigData []byte, pk minisign.PublicKey) (*Manifest, error) {
	sig, err := minisign.ParseSignature(sigData)
	if err != nil {
		return nil, err
	}
	if err := minisign.Verify(pk, manifestData, sig); err != nil {
		return nil, err
	}
	return ParseManifest(manifestData)
}

// CheckTarball verifies the tarball's size and SHA-256 against the manifest.
func CheckTarball(dir string, m *Manifest) error {
	sum, size, err := hashFile(filepath.Join(dir, m.Tarball.Path))
	if err != nil {
		return err
	}
	if size != m.Tarball.Size || sum != m.Tarball.SHA256 {
		return fmt.Errorf("tarball %s does not match the manifest", m.Tarball.Path)
	}
	return nil
}

// Extract unpacks the (already hash-checked) tarball into dest, which must
// not exist yet. Only files listed in the manifest are accepted, each must
// match its listed size and hash, and none may be missing.
func Extract(dir string, m *Manifest, dest string) (err error) {
	if err := os.Mkdir(dest, 0o755); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dest)
		}
	}()

	want := make(map[string]File, len(m.Files))
	for _, f := range m.Files {
		want[f.Path] = f
	}

	tf, err := os.Open(filepath.Join(dir, m.Tarball.Path))
	if err != nil {
		return err
	}
	defer tf.Close()
	gz, err := gzip.NewReader(tf)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	got := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		f, ok := want[hdr.Name]
		if !ok || hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("tarball entry %q is not in the manifest", hdr.Name)
		}
		if got[hdr.Name] {
			return fmt.Errorf("tarball entry %q appears twice", hdr.Name)
		}
		got[hdr.Name] = true
		if err := extractFile(tr, f, dest); err != nil {
			return err
		}
	}
	for p := range want {
		if !got[p] {
			return fmt.Errorf("file %q from the manifest is missing in the tarball", p)
		}
	}
	return nil
}

func extractFile(r io.Reader, f File, dest string) error {
	target := filepath.Join(dest, filepath.FromSlash(f.Path))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if f.Mode == "0755" {
		mode = 0o755
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	h := sha256.New()
	// Read one byte past the expected size so an oversized entry is caught.
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(r, f.Size+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != f.Size || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		return fmt.Errorf("file %q does not match the manifest", f.Path)
	}
	return os.Chmod(target, mode)
}

func readLimited(p string, limit int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", filepath.Base(p), limit)
	}
	return data, nil
}
