//go:build mage

// Shipmates build targets. Run with an installed mage binary
// (`mage <target>`) or with zero install via `go run mage.go <target>`.
// Target names are case-insensitive; `mage -l` lists them.
package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
)

const (
	cgroupLauncherBin = "dist/shipmates-cgroup-launcher"
	runtimeRelease    = "shipmates-runtime-v1"
)

var linuxAmd64 = map[string]string{"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0"}

func buildLinux(out, pkg string) error {
	if err := os.MkdirAll("dist", 0o755); err != nil {
		return err
	}
	if err := sh.RunWith(linuxAmd64, "go", "build", "-trimpath", "-buildvcs=false", "-o", out, pkg); err != nil {
		return err
	}
	return os.Chmod(out, 0o755)
}

// CgroupLauncher builds the Linux cgroup delegation helper into dist/.
func CgroupLauncher() error {
	return buildLinux(cgroupLauncherBin, "./tools/shipmates-cgroup-launcher")
}

// CgroupLauncherVerify builds the helper and checks it against the pinned
// manifest at tools/shipmates-cgroup-launcher/manifest.sha256.
func CgroupLauncherVerify() error {
	mg.Deps(CgroupLauncher)
	return sh.Run("sha256sum", "-c", "tools/shipmates-cgroup-launcher/manifest.sha256")
}

// M3Provision builds the M3 provisioning binary into dist/.
func M3Provision() error {
	return buildLinux("dist/shipmates-m3-provision", "./cmd/shipmates-m3-provision")
}

// M3QualifierRun builds the M3 qualifier runner into dist/.
func M3QualifierRun() error {
	return buildLinux("dist/shipmates-m3-qualifier-run", "./cmd/shipmates-m3-qualifier-run")
}

// InstallerPayloads regenerates the embedded installer payload assets.
func InstallerPayloads() error {
	return sh.Run("tools/generate-installer-payloads.sh")
}

// InstallerManifest emits the same manifest that the binary validates at
// install time into dist/. The file is an offline audit/release input; it
// does not install or contact a host.
func InstallerManifest() error {
	if err := os.MkdirAll("dist", 0o755); err != nil {
		return err
	}
	out, err := sh.Output("go", "run", "./cmd/shipmates-installer-manifest")
	if err != nil {
		return err
	}
	return os.WriteFile("dist/shipmates-installer-manifest.json", []byte(out+"\n"), 0o644)
}

// InstallerCheck runs the offline installer package audit script.
func InstallerCheck() error {
	return sh.Run("bash", "scripts/check-installer-package.sh")
}

// Release produces the offline reproducible runtime archive for the public
// `sudo shipmates install` workflow. It only writes dist/ and never
// installs, starts, provisions, contacts Fleet, or invokes a service
// manager. SOURCE_DATE_EPOCH is required so archive bytes and checksums
// are reproducible.
func Release() error {
	epoch := os.Getenv("SOURCE_DATE_EPOCH")
	if epoch == "" {
		return errors.New("SOURCE_DATE_EPOCH is required")
	}
	stage := filepath.Join("dist", runtimeRelease)
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}
	if err := buildLinux(filepath.Join(stage, "shipmates"), "./"); err != nil {
		return err
	}
	manifest, err := sh.Output("go", "run", "./cmd/shipmates-installer-manifest")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, "shipmates-installer-manifest.json"), []byte(manifest+"\n"), 0o644); err != nil {
		return err
	}
	for _, doc := range []string{"README.md", "LICENSE", "docs/installer-platforms.md"} {
		if err := copyFile(doc, filepath.Join(stage, filepath.Base(doc))); err != nil {
			return err
		}
	}
	if err := os.Chmod(filepath.Join(stage, "shipmates"), 0o755); err != nil {
		return err
	}
	if err := writeSHA256Sums(stage, filepath.Join("dist", runtimeRelease+".sha256")); err != nil {
		return err
	}
	return sh.Run("tar",
		"--sort=name", "--mtime=@"+epoch,
		"--owner=0", "--group=0", "--numeric-owner",
		"-C", "dist", "-czf", filepath.Join("dist", runtimeRelease+".tar.gz"), runtimeRelease)
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

// writeSHA256Sums emits `sha256sum`-compatible lines ("<hex>  <dir>/<name>")
// for every file in stage, sorted by name, relative to dist/ so the file
// verifies with `(cd dist && sha256sum -c <name>.sha256)`.
func writeSHA256Sums(stage, outPath string) error {
	entries, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	for _, name := range names {
		f, err := os.Open(filepath.Join(stage, name))
		if err != nil {
			return err
		}
		h := sha256.New()
		_, cpErr := io.Copy(h, f)
		f.Close()
		if cpErr != nil {
			return cpErr
		}
		if _, err := fmt.Fprintf(out, "%x  %s\n", h.Sum(nil), filepath.ToSlash(filepath.Join(filepath.Base(stage), name))); err != nil {
			return err
		}
	}
	return nil
}
