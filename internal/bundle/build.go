package bundle

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Build packs every regular file under stageDir into outDir as the release
// tarball and writes the matching (unsigned) manifest.json next to it.
// The tarball is deterministic: sorted entries, fixed owner and mtime.
func Build(stageDir, outDir, version string, createdAt time.Time, rollback bool) (*Manifest, error) {
	if _, err := ParseVersion(version); err != nil {
		return nil, err
	}
	files, err := collect(stageDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}

	createdAt = createdAt.UTC().Truncate(time.Second)
	tarPath := filepath.Join(outDir, TarballName(version))
	if err := writeTarball(tarPath, stageDir, files, createdAt); err != nil {
		return nil, err
	}
	tarHash, tarSize, err := hashFile(tarPath)
	if err != nil {
		return nil, err
	}

	m := &Manifest{
		Name:      Name,
		Version:   version,
		CreatedAt: createdAt,
		Rollback:  rollback,
		Tarball:   File{Path: TarballName(version), SHA256: tarHash, Size: tarSize},
		Files:     files,
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("built manifest is invalid: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(outDir, ManifestFile), append(data, '\n'), 0o644); err != nil {
		return nil, err
	}
	return m, nil
}

func collect(stageDir string) ([]File, error) {
	var files []File
	err := filepath.WalkDir(stageDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: only regular files can go in a bundle", p)
		}
		rel, err := filepath.Rel(stageDir, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		sum, size, err := hashFile(p)
		if err != nil {
			return err
		}
		mode := "0644"
		if info.Mode().Perm()&0o111 != 0 {
			mode = "0755"
		}
		files = append(files, File{Path: filepath.ToSlash(rel), SHA256: sum, Size: size, Mode: mode})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, err
}

func writeTarball(tarPath, stageDir string, files []File, mtime time.Time) (err error) {
	out, err := os.Create(tarPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	gz := gzip.NewWriter(out)
	gz.ModTime = mtime
	tw := tar.NewWriter(gz)
	for _, f := range files {
		mode := int64(0o644)
		if f.Mode == "0755" {
			mode = 0o755
		}
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     f.Path,
			Size:     f.Size,
			Mode:     mode,
			ModTime:  mtime,
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		src, err := os.Open(filepath.Join(stageDir, filepath.FromSlash(f.Path)))
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, src)
		src.Close()
		if err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
