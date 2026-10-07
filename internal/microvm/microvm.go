// Package microvm provisions GitHub Actions runners inside microsandbox microVMs.
package microvm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"sync"
	"time"

	msb "github.com/superradcompany/microsandbox/sdk/go"
)

const (
	// LabelOwner identifies the daemon identity that owns a VM. Orphan
	// cleanup matches on this label only.
	LabelOwner = "willet.owner"
	// LabelScaleSet records the scale set name for humans (msb ls); it is
	// not unique across orgs or runner groups, so it is never used to select VMs.
	LabelScaleSet = "willet.scaleset"
)

// Config describes the shape of every runner VM.
type Config struct {
	// Image is the OCI image containing the actions runner.
	Image string
	// RegistryUsername and RegistryPassword authenticate image pulls from a
	// private registry. microsandbox uses them on the host for the pull and
	// never persists them in its catalog or passes them to the guest.
	RegistryUsername string
	RegistryPassword string
	// Docker runs a Docker daemon in each VM, on a DockerDiskMiB ext4 disk,
	// for container jobs, service containers and Docker actions.
	Docker        bool
	DockerDiskMiB uint32
	// DockerRegistryMirror is a Docker Hub pull-through cache for the VMs'
	// Docker daemons, as returned by ParseRegistryMirror. A mirror on
	// host.microsandbox.internal gets its port on the host allowed. Empty
	// means none.
	DockerRegistryMirror string
	// NetworkAllow adds egress exceptions to microsandbox's default policy,
	// which allows only the public internet and DNS.
	NetworkAllow []AllowRule
	// DisableDNSRebindProtection lets private DNS answers reach the guest, so
	// port-restricted NetworkAllow entries also work for hosts reached by
	// name. Connections remain limited by the policy. Off by default.
	DisableDNSRebindProtection bool
	// CPUs is the number of vCPUs per VM.
	CPUs uint8
	// MemoryMiB is the guest memory per VM.
	MemoryMiB uint32
	// DiskMiB is the size of the writable root overlay. Zero uses the runtime default.
	DiskMiB uint32
	// MaxDuration hard-caps the lifetime of a VM. Zero means unlimited.
	MaxDuration time.Duration
	// CreateTimeout bounds creating one VM, including any image pull. Zero
	// means 10 minutes.
	CreateTimeout time.Duration
	// User is the guest user that runs the runner process.
	User string
	// RunnerDir is the directory that contains run.sh inside the image.
	RunnerDir string
	// OwnerID is the owning daemon's identity (see package owner). VMs are
	// labelled with it so orphans can be found after a crash without touching
	// VMs owned by anyone else.
	OwnerID string
	// ScaleSetName is recorded as an informational label.
	ScaleSetName string
	// Env is extra environment passed to the runner process.
	Env map[string]string
	// RunnerOutput logs each line the runner process writes, at info level.
	// The output is not masked the way GitHub masks secrets in job logs.
	RunnerOutput bool
}

// Validate reports configuration that would produce broken or unbounded VMs.
func (c Config) Validate() error {
	switch {
	case c.Image == "":
		return errors.New("image is required")
	case c.CPUs == 0 || c.MemoryMiB == 0:
		return errors.New("CPUs and memory must be positive")
	case c.DockerRegistryMirror != "" && !c.Docker:
		return errors.New("a Docker registry mirror needs Docker enabled")
	case c.Docker && c.DockerDiskMiB < 1024:
		return fmt.Errorf("Docker disk must be at least 1024 MiB (got %d)", c.DockerDiskMiB)
	case c.CreateTimeout < 0:
		return fmt.Errorf("create timeout must not be negative (got %s)", c.CreateTimeout)
	case c.MaxDuration < 0:
		// A negative value would silently disable the lifetime cap.
		return fmt.Errorf("max duration must not be negative (got %s); use 0 for no limit", c.MaxDuration)
	case c.User == "":
		return errors.New("runner user is required")
	case !path.IsAbs(c.RunnerDir):
		return fmt.Errorf("runner dir %q must be an absolute path", c.RunnerDir)
	case (c.RegistryUsername == "") != (c.RegistryPassword == ""):
		return errors.New("registry username and password must be set together")
	case c.OwnerID == "":
		return errors.New("owner ID is required")
	case c.ScaleSetName == "":
		return errors.New("scale set name is required")
	}
	return nil
}

// Provisioner creates runner VMs.
type Provisioner struct {
	cfg    Config
	logger *slog.Logger
	// extra is appended to every sandbox's options; used by tests.
	extra []msb.SandboxOption
	// nameservers overrides the guest's upstream DNS; used by tests.
	nameservers []string

	// auxMu serializes temporary VMs; pendingAux is one whose teardown failed.
	auxMu      sync.Mutex
	pendingAux sandbox
	rt         imageRuntime

	// refreshMu serializes Refresh. imageMu guards image and stale.
	refreshMu sync.Mutex
	imageMu   sync.Mutex
	// image is the pinned reference job VMs boot from; empty until the first
	// successful Refresh, in which case the configured image is used.
	image string
	// stale holds superseded pinned images not yet removed from the cache.
	stale []string
}

// New prepares the microsandbox runtime and returns a Provisioner.
func New(ctx context.Context, cfg Config, logger *slog.Logger) (*Provisioner, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid VM config: %w", err)
	}
	rt, err := alignRuntime(ctx, logger, msb.SDKVersion(), sdkRuntimeSetup)
	if err != nil {
		return nil, fmt.Errorf("ensure microsandbox runtime: %w", err)
	}
	logger.Info("microsandbox runtime ready", slog.String("msb", rt.MSBPath), slog.String("version", msb.SDKVersion()))
	p := &Provisioner{cfg: cfg, logger: logger}
	p.rt = sandboxRuntime{p}
	return p, nil
}

// CleanupOrphans destroys VMs left behind by a previous run with the same
// owner ID. Callers must hold the owner lock (package owner): only then is
// every VM with this owner label guaranteed to belong to a dead process.
func (p *Provisioner) CleanupOrphans(ctx context.Context) error {
	var errs []error
	cursor := ""
	for {
		opts := []msb.SandboxListOption{msb.WithListLabels(map[string]string{LabelOwner: p.cfg.OwnerID})}
		if cursor != "" {
			opts = append(opts, msb.WithListCursor(cursor))
		}
		page, err := msb.ListSandboxesWith(ctx, opts...)
		if err != nil {
			return fmt.Errorf("list sandboxes: %w", err)
		}
		for _, h := range page.Sandboxes {
			p.logger.Info("Destroying orphaned runner VM", slog.String("name", h.Name()), slog.String("status", string(h.Status())))
			if err := h.Destroy(ctx, msb.WithDestroyForce()); err != nil && !msb.IsKind(err, msb.ErrSandboxNotFound) {
				errs = append(errs, fmt.Errorf("destroy %s: %w", h.Name(), err))
			}
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			return errors.Join(errs...)
		}
		cursor = *page.NextCursor
	}
}

func (p *Provisioner) sandboxOptions(name, image string) []msb.SandboxOption {
	opts := []msb.SandboxOption{
		msb.WithImage(image),
		msb.WithCPUs(p.cfg.CPUs),
		msb.WithMemory(p.cfg.MemoryMiB),
		msb.WithHostname(name),
		msb.WithLabel(LabelOwner, p.cfg.OwnerID),
		msb.WithLabel(LabelScaleSet, p.cfg.ScaleSetName),
		// Ephemeral: the runtime drops all on-disk state once the VM stops.
		msb.WithEphemeral(true),
		msb.WithQuietLogs(),
	}
	if p.cfg.DiskMiB > 0 {
		opts = append(opts, msb.WithRootDisk(msb.RootDisk.Managed(p.cfg.DiskMiB)))
	}
	if p.cfg.MaxDuration > 0 {
		opts = append(opts, msb.WithMaxDuration(p.cfg.MaxDuration))
	}
	var hostPorts []string
	if port := mirrorHostPort(p.cfg.DockerRegistryMirror); port != "" {
		hostPorts = append(hostPorts, port)
	}
	if policy := networkPolicy(p.cfg.NetworkAllow, hostPorts, p.cfg.DisableDNSRebindProtection, p.nameservers); policy != nil {
		opts = append(opts, msb.WithNetwork(policy))
	}
	if p.cfg.RegistryUsername != "" {
		opts = append(opts, msb.WithRegistryAuth(msb.RegistryAuth{Username: p.cfg.RegistryUsername, Password: p.cfg.RegistryPassword}))
	}
	return append(opts, p.extra...)
}

// Start boots a VM and launches the runner inside it with the given JIT config.
// On success the VM is running. On error the VM is usually nil, but if the
// runner failed to launch and the VM could not be destroyed, Start returns
// that VM, already exited, together with the error: it still exists and the
// caller must keep it and destroy it later.
func (p *Provisioner) Start(ctx context.Context, name, jitConfig string) (*VM, error) {
	// Pinned images are primed by Refresh, so this never pulls in practice.
	opts := append(p.sandboxOptions(name, p.ImageRef()), msb.WithPullPolicy(msb.PullPolicyIfMissing))
	sb, left, err := p.createSandbox(ctx, name, append(opts, p.dockerOptions()...)...)
	if err != nil {
		err = fmt.Errorf("create sandbox: %w", err)
		if left != nil {
			return p.exitedVM(name, left, err), err
		}
		return nil, err
	}
	if p.cfg.Docker {
		// Docker must be up before the runner can accept a container job.
		if err := p.startDocker(ctx, sb); err != nil {
			return p.abortStart(ctx, name, sb, err)
		}
	}

	// The JIT config is passed only to the runner process rather than as
	// sandbox env, so it is never persisted in the microsandbox catalog.
	env := make(map[string]string, len(p.cfg.Env)+1)
	for k, v := range p.cfg.Env {
		env[k] = v
	}
	env["ACTIONS_RUNNER_INPUT_JITCONFIG"] = jitConfig
	p.runnerDockerEnv(env)

	exec, err := sb.ExecStream(ctx, p.cfg.RunnerDir+"/run.sh", nil,
		msb.WithExecUser(p.cfg.User),
		msb.WithExecCwd(p.cfg.RunnerDir),
		msb.WithExecEnv(env),
	)
	if err != nil {
		return p.abortStart(ctx, name, sb, fmt.Errorf("start runner process: %w", err))
	}

	vm := &VM{
		name:   name,
		sb:     sb,
		exec:   exec,
		logger: p.logger.With(slog.String("runner", name)),
		done:   make(chan struct{}),
	}
	go vm.pump(p.cfg.RunnerOutput)
	return vm, nil
}

// abortStart destroys a sandbox whose runner failed to launch. If that fails
// too, the sandbox still exists: it is returned as an already-exited VM along
// with the error, so the caller keeps it accounted for and retries the
// teardown, instead of the VM silently outliving its capacity slot.
func (p *Provisioner) abortStart(ctx context.Context, name string, sb sandbox, cause error) (*VM, error) {
	if err := destroySandbox(context.WithoutCancel(ctx), sb); err != nil {
		err = fmt.Errorf("%w; destroying the VM also failed, will retry: %v", cause, err)
		return p.exitedVM(name, sb, err), err
	}
	_ = sb.Close()
	return nil, cause
}

// exitedVM wraps a sandbox that exists but runs no runner, so the caller can
// keep it and retry its teardown.
func (p *Provisioner) exitedVM(name string, sb sandbox, cause error) *VM {
	vm := &VM{name: name, sb: sb, logger: p.logger.With(slog.String("runner", name)), done: make(chan struct{}), exitCode: -1, exitErr: cause}
	close(vm.done)
	return vm
}

// Temporary VMs (image pull, probe, connectivity check) are created one at a
// time, under auxMu. One whose teardown fails is kept in pendingAux, and no
// other temporary VM is created until its teardown succeeds, so at most one
// extra VM ever exists beyond the runners.

// withTempVM is the only way temporary VMs are made. Under auxMu it clears a
// leftover VM first, creates a new one, runs use on it, and destroys it. If
// that teardown fails the VM is kept for retry and the teardown error is
// returned too, alongside any error from use.
func withTempVM[S sandbox](ctx context.Context, p *Provisioner, name string, create func() (S, error), use func(S) error) error {
	p.auxMu.Lock()
	defer p.auxMu.Unlock()
	if err := p.clearPendingAux(ctx); err != nil {
		return err
	}
	sb, err := create()
	if err != nil {
		return err
	}
	useErr := use(sb)
	relErr := p.releaseAux(ctx, name, sb)
	switch {
	case useErr != nil && relErr != nil:
		return fmt.Errorf("%w (and %w)", useErr, relErr)
	case useErr != nil:
		return useErr
	default:
		return relErr
	}
}

// clearPendingAux retries the teardown of a temporary VM left over from an
// earlier failure. Callers hold auxMu.
func (p *Provisioner) clearPendingAux(ctx context.Context) error {
	if p.pendingAux == nil {
		return nil
	}
	if err := destroySandbox(ctx, p.pendingAux); err != nil {
		return fmt.Errorf("a previous temporary VM could not be destroyed, so no new one is started: %w", err)
	}
	_ = p.pendingAux.Close()
	p.pendingAux = nil
	return nil
}

// releaseAux destroys a temporary VM, keeping it in pendingAux if that fails.
// Callers hold auxMu.
func (p *Provisioner) releaseAux(ctx context.Context, name string, sb sandbox) error {
	if err := destroySandbox(context.WithoutCancel(ctx), sb); err != nil {
		p.pendingAux = sb
		return fmt.Errorf("destroy temporary VM %s: %w", name, err)
	}
	_ = sb.Close()
	return nil
}

// Close destroys any temporary VM left over from a failed teardown. Call it
// on shutdown; runner VMs are the scaler's to destroy.
func (p *Provisioner) Close(ctx context.Context) error {
	p.auxMu.Lock()
	defer p.auxMu.Unlock()
	return p.clearPendingAux(ctx)
}

// sandbox is the part of *msb.Sandbox that VM lifecycle management uses.
type sandbox interface {
	Destroy(ctx context.Context, opts ...msb.DestroyOption) error
	Close() error
}

// VM is a running runner VM.
type VM struct {
	name   string
	sb     sandbox
	exec   *msb.ExecHandle
	logger *slog.Logger

	done     chan struct{}
	exitCode int
	exitErr  error

	// destroyMu serializes Destroy; destroyed is set only once the sandbox is
	// confirmed gone, so a failed attempt can be retried.
	destroyMu sync.Mutex
	destroyed bool
}

// pump records how the runner process ended and, if logOutput is set,
// forwards its output to the logger.
func (v *VM) pump(logOutput bool) {
	defer close(v.done)
	defer v.exec.Close()

	var stdout, stderr io.Writer = io.Discard, io.Discard
	if logOutput {
		out, errOut := lineLogger(v.logger, "stdout"), lineLogger(v.logger, "stderr")
		defer out.Close()
		defer errOut.Close()
		stdout, stderr = out, errOut
	}

	v.exitCode = -1
	for {
		ev, err := v.exec.Recv(context.Background())
		if err != nil {
			v.exitErr = err
			return
		}
		switch ev.Kind {
		case msb.ExecEventStdout:
			_, _ = stdout.Write(ev.Data)
		case msb.ExecEventStderr:
			_, _ = stderr.Write(ev.Data)
		case msb.ExecEventExited:
			v.exitCode = ev.ExitCode
		case msb.ExecEventFailed:
			v.exitErr = fmt.Errorf("runner process failed to start: %+v", ev.Failure)
		case msb.ExecEventDone:
			return
		}
	}
}

// Name returns the VM (and runner) name.
func (v *VM) Name() string { return v.name }

// Done is closed when the runner process has exited.
func (v *VM) Done() <-chan struct{} { return v.done }

// Result returns the runner exit code and any stream error. Valid after Done is closed.
func (v *VM) Result() (int, error) { return v.exitCode, v.exitErr }

// Destroy stops and removes the VM, falling back to a forced destroy if a
// graceful one fails. It returns nil only once the sandbox is confirmed gone;
// after an error the handle stays open and a later call makes a real new
// attempt. Concurrent calls are serialized. Safe to call after success.
func (v *VM) Destroy(ctx context.Context) error {
	v.destroyMu.Lock()
	defer v.destroyMu.Unlock()
	if v.destroyed {
		return nil
	}
	if err := destroySandbox(ctx, v.sb); err != nil {
		return fmt.Errorf("destroy VM %s: %w", v.name, err)
	}
	v.destroyed = true
	_ = v.sb.Close()
	return nil
}

func destroySandbox(ctx context.Context, sb sandbox) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	graceful := sb.Destroy(ctx, msb.WithDestroyTimeout(5*time.Second))
	if graceful == nil || msb.IsKind(graceful, msb.ErrSandboxNotFound) {
		return nil
	}
	forced := sb.Destroy(ctx, msb.WithDestroyForce())
	if forced == nil || msb.IsKind(forced, msb.ErrSandboxNotFound) {
		return nil
	}
	return errors.Join(graceful, forced)
}

// maxLogLine caps one logged line of runner output. Longer lines are cut
// and marked truncated; logging resumes with the next line.
const maxLogLine = 64 * 1024

// lineLogger returns a writer that emits each line as an info log record.
func lineLogger(logger *slog.Logger, stream string) io.WriteCloser {
	pr, pw := io.Pipe()
	go func() {
		splitLines(pr, maxLogLine, func(line string, truncated bool) {
			if truncated {
				logger.Info(line, slog.String("stream", stream), slog.Bool("truncated", true))
				return
			}
			logger.Info(line, slog.String("stream", stream))
		})
	}()
	return pw
}

// splitLines calls emit for each line in r, without its line ending. Lines
// longer than limit bytes are cut to limit and reported as truncated; the
// rest of such a line is discarded and splitting continues with the next.
// A final line without a newline is emitted too.
func splitLines(r io.Reader, limit int, emit func(line string, truncated bool)) {
	br := bufio.NewReaderSize(r, 4096)
	var buf []byte
	truncated := false
	for {
		chunk, isPrefix, err := br.ReadLine()
		if room := limit - len(buf); len(chunk) > room {
			buf = append(buf, chunk[:max(room, 0)]...)
			truncated = true
		} else {
			buf = append(buf, chunk...)
		}
		if err != nil {
			if len(buf) > 0 {
				emit(string(buf), truncated)
			}
			return
		}
		if !isPrefix {
			emit(string(buf), truncated)
			buf, truncated = buf[:0], false
		}
	}
}
