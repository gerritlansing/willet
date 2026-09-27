package microvm

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	msb "github.com/superradcompany/microsandbox/sdk/go"
)

// Docker support: jobs using `container:`, `services:` or Docker container
// actions need a Docker daemon. As on GitHub-hosted runners, each VM runs its
// own daemon, started before the runner so it's ready when the runner accepts
// a job (container jobs call Docker at pickup, before any step).
//
// The daemon's data lives on a per-VM ext4 disk, because Docker's overlay
// storage can't be stacked on the VM's own OverlayFS root. The disk is sparse
// and removed with the VM. The image must provide dockerd and iptables.

const (
	dockerDataDir = "/var/lib/docker"
	dockerLog     = "/var/log/dockerd.log"
	// dockerReady bounds how long the daemon may take to come up.
	dockerReady = time.Minute
)

// dockerOptions adds the Docker data disk to a VM that will run dockerd.
func (p *Provisioner) dockerOptions() []msb.SandboxOption {
	if !p.cfg.Docker {
		return nil
	}
	return []msb.SandboxOption{msb.WithMounts(map[string]msb.MountConfig{
		dockerDataDir: msb.Mount.Owned(msb.OwnedVolumeOptions{Kind: msb.VolumeKindDisk, SizeMiB: p.cfg.DockerDiskMiB}),
	})}
}

// startDocker starts dockerd as root in the background and waits until it
// answers. On failure the error includes the end of the daemon's log and, for
// known causes, what to change.
func (p *Provisioner) startDocker(ctx context.Context, sb *msb.Sandbox) error {
	root := []msb.ExecOption{msb.WithExecUser("root"), msb.WithExecTimeout(dockerReady + 30*time.Second)}
	// Wait until the daemon answers, but stop as soon as it exits: a broken
	// image then fails in about a second instead of after the full timeout.
	start := fmt.Sprintf(`command -v dockerd >/dev/null || { echo "no dockerd in the image"; exit 3; }
nohup dockerd >%s 2>&1 </dev/null &
pid=$!
end=$(( $(date +%%s) + %d ))
until docker info >/dev/null 2>&1; do
  kill -0 $pid 2>/dev/null || exit 4
  [ $(date +%%s) -lt $end ] || exit 5
  sleep 0.2
done`, dockerLog, int(dockerReady.Seconds()))
	out, err := sb.Exec(ctx, "sh", []string{"-c", start}, root...)
	if err != nil {
		return fmt.Errorf("start Docker: %w", err)
	}
	switch out.ExitCode() {
	case 0:
		return nil
	case 3:
		return fmt.Errorf("start Docker: the runner image has no dockerd; use an image with Docker installed, such as the willet runner image")
	}
	logTail := ""
	if l, err := sb.Exec(ctx, "tail", []string{"-n", "15", dockerLog}, root...); err == nil {
		logTail = strings.TrimSpace(l.Stdout())
	}
	hint := ""
	if strings.Contains(logTail, "iptables not found") {
		hint = "; the runner image has no iptables, which Docker needs for container networking: use an image that includes it, such as the willet runner image"
	}
	what := "exited"
	if out.ExitCode() == 5 {
		what = "not ready after " + dockerReady.String()
	}
	return fmt.Errorf("start Docker: daemon %s%s\ndockerd log:\n%s", what, hint, logTail)
}

// checkDockerAccess verifies the runner user can use the daemon, as jobs will.
func (p *Provisioner) checkDockerAccess(ctx context.Context, sb *msb.Sandbox) error {
	out, err := sb.Exec(ctx, "docker", []string{"info", "--format", "{{.Driver}}"},
		msb.WithExecUser(p.cfg.User), msb.WithExecTimeout(30*time.Second))
	if err != nil {
		return fmt.Errorf("check Docker as %q: %w", p.cfg.User, err)
	}
	if out.ExitCode() != 0 {
		return fmt.Errorf("user %q cannot use Docker (add it to the docker group in the image): %s", p.cfg.User, strings.TrimSpace(out.Stderr()))
	}
	return nil
}

// runnerDockerEnv makes run.sh wait for Docker before starting the runner, as
// a backstop to startDocker.
func (p *Provisioner) runnerDockerEnv(env map[string]string) {
	if p.cfg.Docker {
		env["RUNNER_WAIT_FOR_DOCKER_IN_SECONDS"] = strconv.Itoa(int(dockerReady.Seconds()))
	}
}
