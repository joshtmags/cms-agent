// Command bundletool builds and checks release bundles on a build machine
// (CI or the owner's Mac). It never signs: signing is minisign's job, run by
// scripts/sign-release.sh.
//
//	bundletool build --version 1.2.3 --stage DIR --out DIR [--rollback]
//	bundletool check --version 1.2.3 --dir DIR     re-hash an (unsigned) release before signing it
//	bundletool key-check                           fail if keys/update.pub is still the placeholder
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/joshtmags/cms-agent/internal/bundle"
	"github.com/joshtmags/cms-agent/keys"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		log.Fatal("usage: bundletool build|check|key-check [flags]")
	}
	var err error
	switch os.Args[1] {
	case "build":
		err = build(os.Args[2:])
	case "check":
		err = check(os.Args[2:])
	case "key-check":
		_, err = keys.UpdateKey()
		if err == nil {
			fmt.Println("keys/update.pub is a valid minisign public key")
		}
	default:
		log.Fatal("usage: bundletool build|check|key-check [flags]")
	}
	if err != nil {
		log.Fatalf("bundletool %s: %v", os.Args[1], err)
	}
}

func build(args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	v := fs.String("version", "", "release version, e.g. 1.2.3")
	stage := fs.String("stage", "", "directory with the bundle's files")
	out := fs.String("out", "", "output directory")
	rollback := fs.Bool("rollback", false, "mark as a deliberate downgrade release")
	fs.Parse(args)

	m, err := bundle.Build(*stage, *out, *v, time.Now(), *rollback)
	if err != nil {
		return err
	}
	fmt.Printf("built %s (%d files, tarball %d bytes, sha256 %s)\n", m.Version, len(m.Files), m.Tarball.Size, m.Tarball.SHA256)
	return nil
}

func check(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	v := fs.String("version", "", "expected version")
	dir := fs.String("dir", "", "directory with manifest.json and the tarball")
	fs.Parse(args)

	data, err := os.ReadFile(filepath.Join(*dir, bundle.ManifestFile))
	if err != nil {
		return err
	}
	m, err := bundle.ParseManifest(data)
	if err != nil {
		return err
	}
	if m.Version != *v {
		return fmt.Errorf("manifest is version %s, expected %s", m.Version, *v)
	}
	if err := bundle.CheckTarball(*dir, m); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "bundletool-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := bundle.Extract(*dir, m, filepath.Join(tmp, "x")); err != nil {
		return err
	}
	fmt.Printf("cms-arena-agent %s, built %s%s\n", m.Version, m.CreatedAt.Format(time.RFC3339), map[bool]string{true: " (ROLLBACK release)", false: ""}[m.Rollback])
	for _, f := range m.Files {
		fmt.Printf("  %s  %s  %8d  %s\n", f.Mode, f.SHA256[:12], f.Size, f.Path)
	}
	fmt.Println("tarball and every file match the manifest")
	return nil
}
