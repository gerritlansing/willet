package microvm

import (
	"context"
	"fmt"
	"net/url"
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
//
// A registry mirror (Config.DockerRegistryMirror) lets every VM pull Docker
// Hub images from a pull-through cache instead of downloading them again. A
// VM reaches the host as hostAlias; connections to it arrive on the host's
// loopback, so a cache bound to 127.0.0.1 is enough. Plain http works for a
// mirror without --insecure-registry. Verified against microsandbox v0.7.7
// and Docker 29.

const (
	dockerDataDir = "/var/lib/docker"
	dockerLog     = "/var/log/dockerd.log"
	// dockerReady bounds how long the daemon may take to come up.
	dockerReady = time.Minute
	// hostAlias is the name a VM uses to reach the host.
	hostAlias = "host.microsandbox.internal"
)

// ParseRegistryMirror checks a Docker registry mirror URL and returns it as
// scheme://host[:port]. Docker only accepts a bare registry root.
func ParseRegistryMirror(s string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	switch {
	case err != nil:
		return "", fmt.Errorf("%q: %w", s, err)
	case u.Scheme != "http" && u.Scheme != "https":
		return "", fmt.Errorf("%q: want an http:// or https:// URL", s)
	case u.Hostname() == "":
		return "", fmt.Errorf("%q: missing host", s)
	case u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "":
		return "", fmt.Errorf("%q: want only scheme, host and port, e.g. http://%s:5000", s, hostAlias)
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("%q: invalid port", s)
		}
	}
	return u.Scheme + "://" + u.Host, nil
}

// mirrorHostPort returns the host port a mirror on hostAlias listens on, or
// "" if the mirror is elsewhere (and reachable under the normal policy).
func mirrorHostPort(mirror string) string {
	u, err := url.Parse(mirror)
	if err != nil || !strings.EqualFold(u.Hostname(), hostAlias) {
		return ""
	}
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

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
nohup dockerd ${MIRROR:+--registry-mirror "$MIRROR"} >%s 2>&1 </dev/null &
pid=$!
end=$(( $(date +%%s) + %d ))
until docker info >/dev/null 2>&1; do
  kill -0 $pid 2>/dev/null || exit 4
  [ $(date +%%s) -lt $end ] || exit 5
  sleep 0.2
done`, dockerLog, int(dockerReady.Seconds()))
	env := []msb.ExecOption{msb.WithExecEnv(map[string]string{"MIRROR": p.cfg.DockerRegistryMirror})}
	out, err := sb.Exec(ctx, "sh", []string{"-c", start}, append(root, env...)...)
	if err != nil {
		return fmt.Errorf("start Docker: %w", err)
	}
	switch out.ExitCode() {
	case 0:
		return nil
	case 3:
		return fmt.Errorf("start Docker: the runner image has no dockerd; use an image with Docker installed, such as the willet runner image, or set WILLET_DOCKER=false")
	}
	logTail := ""
	if l, err := sb.Exec(ctx, "tail", []string{"-n", "15", dockerLog}, root...); err == nil {
		logTail = strings.TrimSpace(l.Stdout())
	}
	hint := ""
	if strings.Contains(logTail, "iptables not found") {
		hint = "; the runner image has no iptables, which Docker needs for container networking: use an image that includes it, such as the willet runner image, or set WILLET_DOCKER=false"
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

// checkRegistryMirror reports whether the registry mirror answers from inside
// the VM. Docker falls back to Docker Hub when the mirror fails, so a broken
// mirror only slows pulls down; the caller warns rather than failing. It
// returns nil when there is no mirror or the image has no curl to check with.
func (p *Provisioner) checkRegistryMirror(ctx context.Context, sb *msb.Sandbox) error {
	mirror := p.cfg.DockerRegistryMirror
	if mirror == "" {
		return nil
	}
	// A registry answers /v2/ with 200, or 401 if it requires a login.
	check := `command -v curl >/dev/null || exit 0
code=$(curl -s -o /dev/null -m 10 -w '%{http_code}' "$MIRROR/v2/")
case "$code" in 200|401) exit 0 ;; esac
echo "HTTP status $code"
exit 1`
	out, err := sb.Exec(ctx, "sh", []string{"-c", check}, msb.WithExecEnv(map[string]string{"MIRROR": mirror}), msb.WithExecTimeout(30*time.Second))
	if err != nil {
		return fmt.Errorf("check registry mirror %s: %w", mirror, err)
	}
	if out.ExitCode() != 0 {
		return fmt.Errorf("registry mirror %s did not answer from inside a VM (%s); Docker will pull from Docker Hub instead", mirror, strings.TrimSpace(out.Stdout()))
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
