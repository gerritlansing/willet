package microvm

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	msb "github.com/superradcompany/microsandbox/sdk/go"
	"golang.org/x/mod/semver"
)

// The SDK works only with the runtime release it was built for. Against any
// other, every sandbox creation fails ("no tested sandbox launch contract"),
// and the SDK may first migrate the database in the runtime home past what
// the installed msb can open. EnsureRuntime installs only when no runtime
// exists, so after a willet upgrade the installed one is older than the SDK.
// alignRuntime replaces it before anything touches the database. It never
// downgrades, since an older msb refuses a database a newer one migrated, and
// it never replaces a runtime the operator selected explicitly (MSB_PATH or
// microsandbox configuration).

// runtimeVersionTimeout bounds `msb --version`.
const runtimeVersionTimeout = 30 * time.Second

// runtimeSetup is the microsandbox functionality alignRuntime needs,
// separated so tests can fake it.
type runtimeSetup struct {
	ensure  func(context.Context) (msb.ResolvedRuntime, error)
	install func(context.Context) (msb.ResolvedRuntime, error)
	version func(ctx context.Context, msbPath string) (string, error)
}

// sdkRuntime is the runtimeSetup for the runtime config resolves to; the zero
// config is the default runtime home.
func sdkRuntime(config msb.RuntimeConfig) runtimeSetup {
	return runtimeSetup{
		ensure: func(ctx context.Context) (msb.ResolvedRuntime, error) {
			return msb.EnsureRuntime(ctx, config, msb.InstallOptions{})
		},
		install: func(ctx context.Context) (msb.ResolvedRuntime, error) {
			return msb.InstallRuntime(ctx, config, msb.InstallOptions{Force: true})
		},
		version: msbVersion,
	}
}

// alignRuntime returns an installed runtime at version want, installing or
// upgrading the one in the runtime home as needed.
func alignRuntime(ctx context.Context, logger *slog.Logger, want string, s runtimeSetup) (msb.ResolvedRuntime, error) {
	rt, err := s.ensure(ctx)
	if err != nil {
		return rt, err
	}
	have, err := s.version(ctx, rt.MSBPath)
	if err != nil {
		return rt, err
	}
	if have == want {
		return rt, nil
	}
	if !semver.IsValid("v"+have) || semver.Compare("v"+have, "v"+want) > 0 {
		return rt, fmt.Errorf("%s is microsandbox %s, but this willet needs %s: upgrade willet, or give it its own runtime home with MSB_HOME", rt.MSBPath, have, want)
	}
	if rt.Origin != msb.RuntimeOriginHome && rt.Origin != msb.RuntimeOriginInstalled {
		return rt, fmt.Errorf("%s (selected by %s) is microsandbox %s, but this willet needs %s", rt.MSBPath, rt.Origin, have, want)
	}
	logger.Info("Updating microsandbox runtime", slog.String("msb", rt.MSBPath), slog.String("from", have), slog.String("to", want))
	rt, err = s.install(ctx)
	if err != nil {
		return rt, fmt.Errorf("update microsandbox runtime from %s to %s: %w", have, want, err)
	}
	if have, err = s.version(ctx, rt.MSBPath); err != nil {
		return rt, err
	}
	if have != want {
		return rt, fmt.Errorf("updated microsandbox runtime %s reports version %s, want %s", rt.MSBPath, have, want)
	}
	return rt, nil
}

// msbVersion runs `msb --version`, which prints e.g. "msb 0.7.7".
func msbVersion(ctx context.Context, msbPath string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, runtimeVersionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, msbPath, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", msbPath, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 || fields[0] != "msb" {
		return "", fmt.Errorf("%s --version: unexpected output %q", msbPath, strings.TrimSpace(string(out)))
	}
	return fields[1], nil
}
