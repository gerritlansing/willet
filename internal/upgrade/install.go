package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// Options configures Run.
type Options struct {
	// Tag selects a release; empty means the latest.
	Tag string
	// Current is this binary's version.
	Current string
	// Target is the executable to replace.
	Target string
	// Check reports what would happen without changing anything.
	Check bool
	// Force installs even if the release is not newer than Current.
	Force  bool
	GOOS   string
	GOARCH string
	Log    io.Writer
}

// Result describes what Run did.
type Result struct {
	Tag       string
	Installed bool
}

// Run installs a verified release over Target. The new binary is written and
// checked beside Target first, then renamed into place, so a failure leaves
// the old binary untouched and a running daemon keeps its mapped image.
func (c *Client) Run(ctx context.Context, o Options) (Result, error) {
	if o.Tag != "" && !semver.IsValid(o.Tag) {
		return Result{}, fmt.Errorf("%q is not a version tag like v1.2.3", o.Tag)
	}
	rel, err := c.Release(ctx, o.Tag)
	if err != nil {
		return Result{}, fmt.Errorf("find release: %w", err)
	}
	res := Result{Tag: rel.Tag}
	logf := func(format string, a ...any) { fmt.Fprintf(o.Log, format+"\n", a...) }

	if !semver.IsValid(o.Current) && !o.Force {
		return res, fmt.Errorf("this is a development build (%s); use --force to replace it with %s", o.Current, rel.Tag)
	}
	if semver.IsValid(o.Current) && !o.Force && semver.Compare(rel.Tag, o.Current) <= 0 {
		if o.Tag == "" {
			logf("willet %s is up to date", o.Current)
			return res, nil
		}
		return res, fmt.Errorf("%s is not newer than %s; use --force to install it anyway", rel.Tag, o.Current)
	}
	if o.Check {
		logf("willet %s is available (installed: %s)", rel.Tag, o.Current)
		return res, nil
	}

	asset := AssetName(rel.Tag, o.GOOS, o.GOARCH)
	assetURL, ok := rel.Assets[asset]
	if !ok {
		return res, fmt.Errorf("release %s has no %s", rel.Tag, asset)
	}

	// Create the staging file first so an unwritable directory fails before
	// any download.
	dir := filepath.Dir(o.Target)
	staged, err := os.CreateTemp(dir, ".willet-upgrade-*")
	if err != nil {
		return res, fmt.Errorf("stage new binary (installing needs write access to %s): %w", dir, err)
	}
	defer os.Remove(staged.Name())
	defer staged.Close()

	logf("Downloading %s", asset)
	tarball, digest, err := c.Download(ctx, assetURL, os.TempDir())
	if err != nil {
		return res, fmt.Errorf("download %s: %w", asset, err)
	}
	defer os.Remove(tarball)

	logf("Verifying build attestation")
	verifyFn := c.Verify
	if verifyFn == nil {
		verifyFn = VerifyAttestation
	}
	if err := verifyFn(ctx, c, rel.Tag, digest); err != nil {
		return res, fmt.Errorf("verify %s: %w", asset, err)
	}

	if err := ExtractBinary(tarball, staged); err != nil {
		return res, fmt.Errorf("extract %s: %w", asset, err)
	}
	mode := os.FileMode(0o755)
	if fi, err := os.Stat(o.Target); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := staged.Chmod(mode); err != nil {
		return res, err
	}
	if err := staged.Close(); err != nil {
		return res, err
	}
	if err := checkVersion(ctx, staged.Name(), rel.Tag); err != nil {
		return res, err
	}
	if err := os.Rename(staged.Name(), o.Target); err != nil {
		return res, fmt.Errorf("replace %s: %w", o.Target, err)
	}
	logf("Installed willet %s to %s", rel.Tag, o.Target)
	res.Installed = true
	return res, nil
}

// checkVersion runs the staged binary to make sure it starts and is the
// release it should be.
func checkVersion(ctx context.Context, bin, tag string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return fmt.Errorf("new binary does not run: %w", err)
	}
	if !strings.Contains(string(out), tag) {
		return fmt.Errorf("new binary reports %q, want %s", strings.TrimSpace(string(out)), tag)
	}
	return nil
}

// Executable returns the real path of the running binary.
func Executable() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	p, err = filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	if p == "" {
		return "", errors.New("cannot locate the willet executable")
	}
	return p, nil
}
