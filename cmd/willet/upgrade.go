package main

import (
	"fmt"
	"net/http"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/gerritlansing/willet/internal/upgrade"
)

func newUpgradeCommand() *cobra.Command {
	var (
		tag       string
		service   string
		check     bool
		force     bool
		noRestart bool
		terminate bool
	)
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Install the latest willet release and restart the service",
		Long: `upgrade downloads the latest willet release from GitHub, verifies that its
tarball was built by this repository's release workflow (Sigstore build
provenance), replaces this executable, and restarts the systemd service.

The restart is a drain: the daemon takes no new jobs, lets running jobs
finish (up to its --drain-timeout), then exits and systemd starts the new
version. With --terminate it stops at once and destroys running jobs. A
daemon older than this feature does not drain; it always stops at once.
The restarted daemon updates the microsandbox runtime to the version the new
release was built for.

Installing into a root-owned directory such as /usr/local/bin needs root, for
example: sudo willet upgrade`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := interruptContext(cmd.Context())
			defer stop()
			target, err := upgrade.Executable()
			if err != nil {
				return err
			}
			c := &upgrade.Client{HTTP: &http.Client{Timeout: 5 * time.Minute}}
			res, err := c.Run(ctx, upgrade.Options{
				Tag:     tag,
				Current: version,
				Target:  target,
				Check:   check,
				Force:   force,
				GOOS:    runtime.GOOS,
				GOARCH:  runtime.GOARCH,
				Log:     cmd.ErrOrStderr(),
			})
			if err != nil || !res.Installed {
				return err
			}
			if noRestart {
				fmt.Fprintf(cmd.ErrOrStderr(), "Not restarting; restart willet to run %s\n", res.Tag)
				return nil
			}
			u := unit{name: service, out: cmd.ErrOrStderr()}
			if !u.running(ctx) {
				return nil
			}
			return u.restart(ctx, terminate)
		},
	}
	f := cmd.Flags()
	f.StringVar(&tag, "to", "", "install this release tag, e.g. v1.2.3, instead of the latest")
	f.BoolVar(&check, "check", false, "only report whether a newer release exists")
	f.BoolVar(&force, "force", false, "install even if the release is not newer than this binary")
	f.BoolVar(&noRestart, "no-restart", false, "do not restart the service after installing")
	f.BoolVar(&terminate, "terminate", false, "restart at once, destroying running jobs, instead of waiting for them")
	f.StringVar(&service, "service", defaultService, "systemd unit to restart")
	return cmd
}
