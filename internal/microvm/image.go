package microvm

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	msb "github.com/superradcompany/microsandbox/sdk/go"
)

// Runner images are pinned by digest so every job in a refresh window runs the
// exact same image, and so a refresh never swaps the image under a running VM.
//
// microsandbox caches images by reference string and, with the default
// if-missing pull policy, never re-checks a tag once it's cached. A mutable tag
// like :latest would therefore never pick up new runner releases, and GitHub
// stops dispatching jobs to runners more than 30 days out of date (automatic
// runner updates are disabled for ephemeral runners). Refresh re-pulls the tag
// on a schedule and moves job VMs to the digest it now resolves to.

// RefreshResult describes the image job VMs use after a refresh.
type RefreshResult struct {
	// Ref is the pinned reference (repo@sha256:...) job VMs now boot from.
	Ref string
	// Previous is the pinned reference used before, "" on the first refresh.
	Previous string
	// Changed reports whether Ref differs from Previous.
	Changed bool
	// RunnerVersion is the actions runner version in the image, if detectable.
	RunnerVersion string
}

// imageRuntime is the microsandbox functionality Refresh needs, separated so
// the refresh logic can be tested without KVM.
type imageRuntime interface {
	// pull fetches the current manifest for ref, bypassing the cache.
	pull(ctx context.Context, name, ref string) error
	// digest returns the manifest digest ref resolves to in the local cache.
	digest(ctx context.Context, ref string) (string, error)
	// probe boots a VM from ref, verifies the runner can start, and returns
	// the runner version ("" if unknown).
	probe(ctx context.Context, name, ref string) (string, error)
	// remove deletes ref from the image cache unless a sandbox still uses it.
	remove(ctx context.Context, ref string) error
}

// ImageRef returns the image reference job VMs currently boot from.
func (p *Provisioner) ImageRef() string {
	p.imageMu.Lock()
	defer p.imageMu.Unlock()
	if p.image == "" {
		return p.cfg.Image
	}
	return p.image
}

// Refresh resolves the configured image to a digest, checks it with a test
// VM, and switches future job VMs to it. On error the previous image stays in
// use. name prefixes the short-lived VMs Refresh boots.
func (p *Provisioner) Refresh(ctx context.Context, name string) (RefreshResult, error) {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()

	ref := p.cfg.Image
	if !isDigestRef(ref) {
		if err := p.rt.pull(ctx, name+"-pull", ref); err != nil {
			return RefreshResult{}, fmt.Errorf("pull %s: %w", ref, err)
		}
		digest, err := p.rt.digest(ctx, ref)
		if err != nil {
			return RefreshResult{}, fmt.Errorf("resolve digest of %s: %w", ref, err)
		}
		if digest == "" {
			return RefreshResult{}, fmt.Errorf("resolve digest of %s: runtime reported no digest", ref)
		}
		ref = repository(ref) + "@" + digest
	}

	// Booting from the pinned reference also caches it under that name, so
	// job VMs never pull.
	version, err := p.rt.probe(ctx, name, ref)
	if err != nil {
		return RefreshResult{}, err
	}

	p.imageMu.Lock()
	old := p.image
	p.image = ref
	// The selected image may be a superseded one coming back (a tag rolled
	// back); it is current again and must not be pruned.
	p.stale = slices.DeleteFunc(p.stale, func(s string) bool { return s == ref })
	if old != "" && old != ref && old != p.cfg.Image && !slices.Contains(p.stale, old) {
		p.stale = append(p.stale, old)
	}
	p.imageMu.Unlock()

	p.pruneStale(ctx)
	return RefreshResult{Ref: ref, Previous: old, Changed: old != ref, RunnerVersion: version}, nil
}

// pruneStale removes superseded images from the cache once no VM uses them.
// Images still in use are kept and retried after the next refresh.
func (p *Provisioner) pruneStale(ctx context.Context) {
	p.imageMu.Lock()
	stale := p.stale
	p.stale = nil
	current := p.image
	p.imageMu.Unlock()

	var keep []string
	for _, ref := range stale {
		if ref == current {
			continue // never delete the image new VMs boot from
		}
		if err := p.rt.remove(ctx, ref); err != nil && !msb.IsKind(err, msb.ErrImageNotFound) {
			p.logger.Debug("Could not remove superseded image yet; will retry after the next refresh", "image", ref, "error", err.Error())
			keep = append(keep, ref)
			continue
		}
		p.logger.Info("Removed superseded runner image", "image", ref)
	}

	p.imageMu.Lock()
	p.stale = append(keep, p.stale...)
	p.imageMu.Unlock()
}

func isDigestRef(ref string) bool { return strings.Contains(ref, "@") }

// repository strips the tag from an image reference. A colon only starts a
// tag after the last slash; earlier ones belong to a registry port.
func repository(ref string) string {
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i]
	}
	return ref
}

// ---------------------------------------------------------------------------
// microsandbox implementation

type sandboxRuntime struct{ p *Provisioner }

func (r sandboxRuntime) pull(ctx context.Context, name, ref string) error {
	p := r.p
	return withTempVM(ctx, p, name, func() (*msb.Sandbox, error) {
		return p.createTempSandbox(ctx, name, append(p.sandboxOptions(name, ref),
			msb.WithPullPolicy(msb.PullPolicyAlways), msb.WithReplace())...)
	}, func(*msb.Sandbox) error { return nil }) // creating it is the pull
}

func (sandboxRuntime) digest(ctx context.Context, ref string) (string, error) {
	h, err := msb.Image.Get(ctx, ref)
	if err != nil {
		return "", err
	}
	return h.ManifestDigest(), nil
}

var runnerVersion = regexp.MustCompile(`(?m)^\d+\.\d+\.\d+$`)

func (r sandboxRuntime) probe(ctx context.Context, name, ref string) (string, error) {
	p := r.p
	var version string
	err := withTempVM(ctx, p, name, func() (*msb.Sandbox, error) {
		opts := append(p.sandboxOptions(name, ref), msb.WithPullPolicy(msb.PullPolicyIfMissing), msb.WithReplace())
		sb, err := p.createTempSandbox(ctx, name, append(opts, p.dockerOptions()...)...)
		if err != nil {
			return nil, fmt.Errorf("boot test VM from %s: %w", ref, err)
		}
		return sb, nil
	}, func(sb *msb.Sandbox) error {
		// Run the check exactly as a real runner starts: same user, same directory.
		as := []msb.ExecOption{msb.WithExecUser(p.cfg.User), msb.WithExecCwd(p.cfg.RunnerDir)}
		out, err := sb.Exec(ctx, "test", []string{"-x", "run.sh"}, as...)
		if err != nil {
			return fmt.Errorf("check image as user %q in %s: %w", p.cfg.User, p.cfg.RunnerDir, err)
		}
		if out.ExitCode() != 0 {
			return fmt.Errorf("image %s: user %q cannot execute %s/run.sh", ref, p.cfg.User, p.cfg.RunnerDir)
		}
		if p.cfg.Docker {
			if err := p.startDocker(ctx, sb); err != nil {
				return fmt.Errorf("image %s: %w", ref, err)
			}
			if err := p.checkDockerAccess(ctx, sb); err != nil {
				return fmt.Errorf("image %s: %w", ref, err)
			}
		}
		// Best effort: custom images may not ship the standard runner layout.
		if out, err := sb.Exec(ctx, "./bin/Runner.Listener", []string{"--version"}, as...); err == nil {
			version = runnerVersion.FindString(out.Stdout())
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return version, nil
}

func (sandboxRuntime) remove(ctx context.Context, ref string) error {
	return msb.Image.Remove(ctx, ref, false)
}
