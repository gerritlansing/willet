package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const defaultService = "willet"

// drainWindow is the default --drain-timeout. A unit whose TimeoutStopSec is
// shorter ends a drain early, because systemd then kills the daemon.
const drainWindow = 6 * time.Hour

func addServiceFlag(cmd *cobra.Command, service *string) {
	cmd.Flags().StringVar(service, "service", defaultService, "systemd unit running willet")
}

func newStopCommand() *cobra.Command {
	var service string
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the willet service after running jobs finish",
		Long: `stop stops the systemd service. The daemon takes no new jobs, lets running
jobs finish (up to its --drain-timeout), then exits and stays stopped.

Use "willet terminate" to stop at once and destroy running jobs.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := interruptContext(cmd.Context())
			defer stop()
			u := unit{name: service, out: cmd.ErrOrStderr()}
			if !u.running(ctx) {
				return nil
			}
			u.warnSettings(ctx)
			fmt.Fprintf(u.out, "Stopping %s; running jobs finish first. `willet terminate` stops now.\n", service)
			if err := u.systemctl(ctx, "stop"); err != nil {
				return err
			}
			fmt.Fprintf(u.out, "Stopped %s\n", service)
			return nil
		},
	}
	addServiceFlag(cmd, &service)
	return cmd
}

func newTerminateCommand() *cobra.Command {
	var service string
	cmd := &cobra.Command{
		Use:   "terminate",
		Short: "Stop the willet service now, destroying running jobs",
		Long: `terminate stops the systemd service without waiting for jobs: the daemon
destroys every runner VM, running jobs included, and exits. It stays stopped.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := interruptContext(cmd.Context())
			defer stop()
			u := unit{name: service, out: cmd.ErrOrStderr()}
			if !u.running(ctx) {
				return nil
			}
			fmt.Fprintf(u.out, "Terminating %s; running jobs are destroyed\n", service)
			if err := u.forceStop(ctx); err != nil {
				return err
			}
			if err := u.systemctl(ctx, "stop"); err != nil { // waits for the stop to finish
				return err
			}
			fmt.Fprintf(u.out, "Stopped %s\n", service)
			return nil
		},
	}
	addServiceFlag(cmd, &service)
	return cmd
}

// unit is a systemd service willet runs as.
type unit struct {
	name string
	out  io.Writer
}

func (u unit) systemctl(ctx context.Context, args ...string) error {
	c := exec.CommandContext(ctx, "systemctl", append(args, u.name)...)
	c.Stdout, c.Stderr = os.Stdout, u.out
	if err := c.Run(); err != nil {
		return fmt.Errorf("systemctl %s %s: %w", strings.Join(args, " "), u.name, err)
	}
	return nil
}

// running reports whether the unit is active, and says why not otherwise. A
// stopped service is left alone: starting one the operator stopped is not
// these commands' job.
func (u unit) running(ctx context.Context) bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		fmt.Fprintln(u.out, "systemctl not found; this command manages a systemd service")
		return false
	}
	if exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", u.name).Run() != nil {
		fmt.Fprintf(u.out, "Service %s is not running\n", u.name)
		return false
	}
	return true
}

// forceStop queues a stop and then sends the daemon a second signal, which
// makes it skip the drain. systemd's stop job sends SIGTERM first, so
// whichever signal arrives first starts the drain and the other ends it.
// The stop job also keeps Restart=always from starting the daemon again.
func (u unit) forceStop(ctx context.Context) error {
	if err := u.systemctl(ctx, "stop", "--no-block"); err != nil {
		return err
	}
	if err := exec.CommandContext(ctx, "systemctl", "kill", "--kill-whom=main", "--signal=SIGINT", u.name).Run(); err != nil {
		fmt.Fprintf(u.out, "Warning: could not signal %s: %v\n", u.name, err)
	}
	return nil
}

// restart restarts the unit, after the new binary is in place. Without
// force the daemon drains first and the call returns once the new daemon is
// starting, which can take as long as the longest job.
func (u unit) restart(ctx context.Context, force bool) error {
	u.warnSettings(ctx)
	if force {
		fmt.Fprintf(u.out, "Restarting %s now; running jobs are destroyed\n", u.name)
		if err := u.systemctl(ctx, "restart", "--no-block"); err != nil {
			return err
		}
		if err := exec.CommandContext(ctx, "systemctl", "kill", "--kill-whom=main", "--signal=SIGINT", u.name).Run(); err != nil {
			fmt.Fprintf(u.out, "Warning: could not signal %s: %v\n", u.name, err)
		}
		fmt.Fprintf(u.out, "Restart queued; systemd starts the new version once %s has stopped\n", u.name)
		return nil
	}
	fmt.Fprintf(u.out, "Restarting %s; running jobs finish first. Ctrl-C stops waiting but leaves the restart queued; `willet terminate` forces it.\n", u.name)
	if err := u.systemctl(ctx, "restart"); err != nil {
		return fmt.Errorf("willet was upgraded, but restarting %s failed: %w", u.name, err)
	}
	fmt.Fprintf(u.out, "Restarted %s\n", u.name)
	return nil
}

// warnSettings warns about unit settings that would defeat a drain.
func (u unit) warnSettings(ctx context.Context) {
	out, err := exec.CommandContext(ctx, "systemctl", "show", "--property=KillMode", "--property=TimeoutStopUSec", u.name).Output()
	if err != nil {
		return
	}
	for _, w := range unitWarnings(string(out), u.name) {
		fmt.Fprintln(u.out, "Warning: "+w)
	}
}

// unitWarnings inspects `systemctl show` output for KillMode and
// TimeoutStopUSec.
func unitWarnings(show, name string) []string {
	var warns []string
	for _, line := range strings.Split(show, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "KillMode":
			if v != "mixed" && v != "process" {
				warns = append(warns, fmt.Sprintf("%s has KillMode=%s, so stopping it also signals the runner VMs and ends running jobs. Set KillMode=mixed in the unit.", name, v))
			}
		case "TimeoutStopUSec":
			d, infinite, err := parseSystemdSpan(v)
			if err == nil && !infinite && d < drainWindow {
				warns = append(warns, fmt.Sprintf("%s has TimeoutStopSec=%s, after which systemd kills it and any jobs still running. If jobs can run longer, set TimeoutStopSec=6h 2min (--drain-timeout plus cleanup time).", name, v))
			}
		}
	}
	return warns
}

var spanPart = regexp.MustCompile(`^(\d+(?:\.\d+)?)(us|ms|s|min|h|d|w|month|y)$`)

// parseSystemdSpan parses the time span format `systemctl show` prints, such
// as "1min 30s" or "6h 2min", or "infinity".
func parseSystemdSpan(s string) (d time.Duration, infinite bool, err error) {
	s = strings.TrimSpace(s)
	if s == "infinity" {
		return 0, true, nil
	}
	if s == "" {
		return 0, false, errors.New("empty time span")
	}
	units := map[string]time.Duration{
		"us": time.Microsecond, "ms": time.Millisecond, "s": time.Second, "min": time.Minute,
		"h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour,
		"month": 30*24*time.Hour + 10*time.Hour + 30*time.Minute, "y": 365*24*time.Hour + 6*time.Hour,
	}
	for _, part := range strings.Fields(s) {
		m := spanPart.FindStringSubmatch(part)
		if m == nil {
			return 0, false, fmt.Errorf("unrecognized time span %q", s)
		}
		n, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return 0, false, err
		}
		d += time.Duration(n * float64(units[m[2]]))
	}
	return d, false, nil
}
