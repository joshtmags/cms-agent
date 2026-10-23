// Package bundle builds and verifies update bundles.
//
// A release is three files:
//
//	manifest.json              version, SHA-256 of the tarball and of every file in it
//	manifest.json.minisig      minisign signature of manifest.json (made on the owner's Mac)
//	cms-arena-agent-<v>.tar.gz the files: agent binary, provision.sh, files/...
//
// Only manifest.json is signed. Everything else is trusted through the hashes
// it lists, so a PC verifies the signature first, then the tarball hash, then
// every extracted file.
package bundle

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	Name          = "cms-arena-agent"
	ManifestFile  = "manifest.json"
	SignatureFile = "manifest.json.minisig"

	maxManifestBytes = 1 << 20
	maxTarballBytes  = 200 << 20
)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Manifest describes one release. It is the only signed file.
type Manifest struct {
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	// Rollback marks a deliberate downgrade release. Without it a PC refuses
	// any version that isn't newer than the one it runs.
	Rollback bool   `json:"rollback,omitempty"`
	Tarball  File   `json:"tarball"`
	Files    []File `json:"files"`
}

// File is one entry in the manifest.
type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	// Mode is "0644" or "0755"; ignored for the tarball entry.
	Mode string `json:"mode,omitempty"`
}

// TarballName is the release asset name for a version.
func TarballName(version string) string {
	return fmt.Sprintf("%s-%s.tar.gz", Name, version)
}

// ParseManifest decodes and validates manifest.json.
func ParseManifest(data []byte) (*Manifest, error) {
	if len(data) > maxManifestBytes {
		return nil, errors.New("manifest: too large")
	}
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	return &m, nil
}

func (m *Manifest) validate() error {
	if m.Name != Name {
		return fmt.Errorf("name is %q, want %q", m.Name, Name)
	}
	if _, err := ParseVersion(m.Version); err != nil {
		return err
	}
	if m.Tarball.Path != TarballName(m.Version) {
		return fmt.Errorf("tarball is %q, want %q", m.Tarball.Path, TarballName(m.Version))
	}
	if !sha256Hex.MatchString(m.Tarball.SHA256) {
		return errors.New("tarball sha256 is not 64 lowercase hex characters")
	}
	if m.Tarball.Size <= 0 || m.Tarball.Size > maxTarballBytes {
		return fmt.Errorf("tarball size %d out of range", m.Tarball.Size)
	}
	if len(m.Files) == 0 {
		return errors.New("no files")
	}
	seen := map[string]bool{}
	for _, f := range m.Files {
		if !validPath(f.Path) {
			return fmt.Errorf("invalid file path %q", f.Path)
		}
		if seen[f.Path] {
			return fmt.Errorf("duplicate file %q", f.Path)
		}
		seen[f.Path] = true
		if !sha256Hex.MatchString(f.SHA256) {
			return fmt.Errorf("%s: sha256 is not 64 lowercase hex characters", f.Path)
		}
		if f.Size < 0 {
			return fmt.Errorf("%s: negative size", f.Path)
		}
		if f.Mode != "0644" && f.Mode != "0755" {
			return fmt.Errorf("%s: mode %q, want 0644 or 0755", f.Path, f.Mode)
		}
	}
	for _, required := range []string{Name, "provision.sh"} {
		if !seen[required] {
			return fmt.Errorf("missing required file %q", required)
		}
	}
	return nil
}

// validPath accepts clean, relative, slash-separated paths with no "..".
func validPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	if path.Clean(p) != p {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == "." {
			return false
		}
	}
	return true
}

// Version is a MAJOR.MINOR.PATCH release number.
type Version [3]int

// ParseVersion parses "1.2.3" (an optional leading "v" is allowed).
func ParseVersion(s string) (Version, error) {
	var v Version
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != 3 {
		return v, fmt.Errorf("version %q is not MAJOR.MINOR.PATCH", s)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || (len(p) > 1 && p[0] == '0') {
			return v, fmt.Errorf("version %q is not MAJOR.MINOR.PATCH", s)
		}
		v[i] = n
	}
	return v, nil
}

// Compare returns -1, 0 or 1.
func (v Version) Compare(o Version) int {
	for i := range v {
		switch {
		case v[i] < o[i]:
			return -1
		case v[i] > o[i]:
			return 1
		}
	}
	return 0
}

// CheckUpgrade enforces anti-rollback: the candidate must be newer than the
// running version, unless the (signed) manifest is a deliberate rollback
// release. Reinstalling the same version is always refused.
func CheckUpgrade(running string, m *Manifest) error {
	cur, err := ParseVersion(running)
	if err != nil {
		return err
	}
	next, err := ParseVersion(m.Version)
	if err != nil {
		return err
	}
	switch cmp := next.Compare(cur); {
	case cmp == 0:
		return fmt.Errorf("version %s is already installed", m.Version)
	case cmp < 0 && !m.Rollback:
		return fmt.Errorf("refusing downgrade from %s to %s (not a signed rollback release)", running, m.Version)
	}
	return nil
}
