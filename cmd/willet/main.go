// Command willet is a GitHub Actions runner scale set that runs every job
// in a fresh microsandbox microVM.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/gerritlansing/willet/internal/microvm"
	"github.com/gerritlansing/willet/internal/owner"
	"github.com/gerritlansing/willet/internal/scaler"
)

// Set with -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	if err := newCommand().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func newCommand() *cobra.Command {
	var cfg Config
	var privateKeyFile, registryPasswordFile, envFile string

	cmd := &cobra.Command{
		Use:   "willet",
		Short: "GitHub Actions runner scale set backed by microsandbox microVMs",
		Long: `willet registers a GitHub Actions runner scale set and runs every job
in a fresh, ephemeral microsandbox microVM.

Every flag can also be set through an environment variable named
` + envPrefix + `<FLAG> (upper case, dashes as underscores), for example
` + envPrefix + `TOKEN or ` + envPrefix + `APP_PRIVATE_KEY, either in the process
environment or in a file passed with --env-file. Command-line flags win over
the environment, which wins over the env file.`,
		Version:       fmt.Sprintf("%s (%s)", version, commit),
		SilenceUsage:  true,
		SilenceErrors: true,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := loadEnv(cmd.Flags(), envFile); err != nil {
				return err
			}
			if privateKeyFile != "" {
				b, err := os.ReadFile(privateKeyFile)
				if err != nil {
					return fmt.Errorf("read --app-private-key-file: %w", err)
				}
				cfg.GitHubApp.PrivateKey = string(b)
			}
			if registryPasswordFile != "" {
				p, err := readPasswordFile(registryPasswordFile)
				if err != nil {
					return fmt.Errorf("read --registry-password-file: %w", err)
				}
				cfg.RegistryPassword = p
			}
			return cfg.Validate()
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			err := run(ctx, cfg)
			if err != nil && ctx.Err() != nil {
				// Asked to stop: whatever was in flight failed because of that.
				return nil
			}
			return err
		},
	}

	f := cmd.Flags()
	f.SortFlags = false
	f.StringVar(&envFile, "env-file", "", "read settings from this env file (also "+envFileVar+")")
	f.StringVar(&cfg.RegistrationURL, "url", "", "GitHub org, repo or enterprise URL to register the scale set in (required)")
	f.StringVar(&cfg.ScaleSetName, "name", "", "scale set name; also the default runs-on label (required)")
	f.StringSliceVar(&cfg.Labels, "labels", nil, "runs-on labels (defaults to --name)")
	f.StringVar(&cfg.RunnerGroup, "runner-group", scaleset.DefaultRunnerGroup, "runner group for the scale set")
	f.IntVar(&cfg.MinRunners, "min-runners", 0, "idle runners to keep warm")
	f.IntVar(&cfg.MaxRunners, "max-runners", 4, "maximum concurrent runner VMs")

	f.StringVar(&cfg.GitHubApp.ClientID, "app-client-id", "", "GitHub App client ID")
	f.Int64Var(&cfg.GitHubApp.InstallationID, "app-installation-id", 0, "GitHub App installation ID")
	f.StringVar(&cfg.GitHubApp.PrivateKey, "app-private-key", "", "GitHub App private key (PEM)")
	f.StringVar(&privateKeyFile, "app-private-key-file", "", "path to the GitHub App private key (PEM)")
	f.StringVar(&cfg.Token, "token", "", "personal access token (alternative to a GitHub App)")

	f.StringVar(&cfg.RunnerImage, "runner-image", "ghcr.io/gerritlansing/willet-runner:latest", "OCI image containing the actions runner; a custom image needs run.sh in --runner-dir, executable by --runner-user")
	f.StringVar(&cfg.RegistryUsername, "registry-username", "", "username for pulling --runner-image from a private registry")
	f.StringVar(&registryPasswordFile, "registry-password-file", "", "path to the password or token for --registry-username")
	f.Uint8Var(&cfg.CPUs, "cpus", 2, "vCPUs per runner VM")
	f.Uint32Var(&cfg.MemoryMiB, "memory", 4096, "memory per runner VM in MiB")
	f.Uint32Var(&cfg.DiskMiB, "disk", 0, "writable root disk per runner VM in MiB (0 = runtime default)")
	f.DurationVar(&cfg.MaxJobDuration, "max-job-duration", 6*time.Hour, "hard cap on a runner VM's lifetime (0 = unlimited)")
	f.DurationVar(&cfg.StartTimeout, "start-timeout", 10*time.Minute, "limit on creating one VM, including any image pull, and on registering its runner")
	f.StringVar(&cfg.RunnerUser, "runner-user", "runner", "guest user that runs the runner")
	f.StringVar(&cfg.RunnerDir, "runner-dir", "/home/runner", "guest directory containing run.sh")
	f.StringSliceVar(&cfg.NetworkAllow, "network-allow", nil, "private IPs or CIDRs runner VMs may reach, each optionally with :port or :port-range, e.g. 10.0.5.10,10.1.0.0/16:443 (VMs reach only the public internet by default)")
	f.BoolVar(&cfg.DNSRebindProtection, "dns-rebind-protection", true, "drop private DNS answers unless --network-allow covers the address without a port; false lets port-restricted entries work by name (see README)")
	f.BoolVar(&cfg.Docker, "docker", true, "run a Docker daemon in each VM for container jobs, service containers and Docker actions; the image needs dockerd and iptables")
	f.Uint32Var(&cfg.DockerDiskMiB, "docker-disk", 20480, "Docker data disk per VM in MiB (sparse; only space actually used is allocated)")
	f.BoolVar(&cfg.SkipWarmup, "skip-warmup", false, "skip pulling the image and booting a test VM at startup; the cached image is used until the first scheduled refresh")
	f.DurationVar(&cfg.ImageRefreshInterval, "image-refresh-interval", 24*time.Hour, "how often to re-pull a tagged image such as :latest and move new runners to it (0 = never; ignored for digest-pinned images)")

	f.BoolVar(&cfg.DeleteOnExit, "delete-on-exit", false, "delete the scale set from GitHub on shutdown")
	f.StringVar(&cfg.LogLevel, "log-level", "info", "debug, info, warn or error")
	f.StringVar(&cfg.LogFormat, "log-format", "text", "text or json")
	f.BoolVar(&cfg.RunnerOutput, "log-runner-output", false, "log each line of runner output at info level; not masked the way GitHub masks secrets")

	return cmd
}

func run(ctx context.Context, cfg Config) error {
	logger := cfg.Logger()

	// Take the single-instance lock before anything that can destroy VMs, so
	// a second daemon for the same scale set exits without side effects.
	ownerID, err := owner.ID(cfg.RegistrationURL, cfg.RunnerGroup, cfg.ScaleSetName)
	if err != nil {
		return err
	}
	stateDir, err := owner.StateDir()
	if err != nil {
		return err
	}
	lock, err := owner.Acquire(stateDir, ownerID)
	if err != nil {
		return err
	}
	defer lock.Release()
	namePrefix := cfg.NamePrefix() + "-" + ownerID[:6]

	// Cleanup runs in phases under one deadline (see lifecycle.go), starting
	// when run returns for any reason. Steps are registered as resources are
	// created, so an early failure only cleans up what exists.
	shutdown := &shutdownSteps{logger: logger.WithGroup("shutdown")}
	defer func() {
		logger.Info("Shutting down", slog.Duration("budget", shutdownBudget))
		shutdown.run(shutdownBudget)
	}()
	if !cfg.DNSRebindProtection {
		logger.Warn("DNS rebinding protection is disabled: runner VMs receive private DNS answers. Connections are still limited to the public internet and --network-allow.")
	}
	logger.Info("Acquired owner lock", slog.String("owner", ownerID), slog.String("stateDir", stateDir))

	prov, err := microvm.New(ctx, microvm.Config{
		Image:            cfg.RunnerImage,
		RegistryUsername: cfg.RegistryUsername,
		RegistryPassword: cfg.RegistryPassword,
		NetworkAllow:     cfg.networkAllow,

		DisableDNSRebindProtection: !cfg.DNSRebindProtection,
		Docker:                     cfg.Docker,
		DockerDiskMiB:              cfg.DockerDiskMiB,
		CPUs:                       cfg.CPUs,
		MemoryMiB:                  cfg.MemoryMiB,
		DiskMiB:                    cfg.DiskMiB,
		MaxDuration:                cfg.MaxJobDuration,
		CreateTimeout:              cfg.StartTimeout,
		User:                       cfg.RunnerUser,
		RunnerDir:                  cfg.RunnerDir,
		OwnerID:                    ownerID,
		ScaleSetName:               cfg.ScaleSetName,
		RunnerOutput:               cfg.RunnerOutput,
	}, logger.WithGroup("microvm"))
	if err != nil {
		return err
	}
	shutdown.add(destroyTempVMStep(prov.Close))
	if !cfg.SkipWarmup {
		logger.Info("Warming up: pulling the runner image and booting a test VM", slog.String("image", cfg.RunnerImage))
		if err := refreshImage(ctx, prov, namePrefix, logger); err != nil {
			return fmt.Errorf("warm-up failed: %w", err)
		}
		// Runners must reach the GitHub server from inside the VM, which for a
		// private GHES needs an explicit --network-allow entry.
		host, port := cfg.serverAddress()
		server := net.JoinHostPort(host, port)
		checked, err := prov.CheckReachable(ctx, namePrefix+"-netcheck", host, port)
		switch {
		case err != nil:
			return fmt.Errorf("connectivity check failed: %w", err)
		case checked:
			logger.Info("Runner VMs can reach the GitHub server", slog.String("server", server))
		default:
			logger.Warn("Could not check whether runner VMs can reach the GitHub server: the runner image has no bash, nc or working curl", slog.String("server", server))
		}
	}

	client, err := cfg.ScalesetClient()
	if err != nil {
		return fmt.Errorf("create scaleset client: %w", err)
	}
	scaleSet, err := ensureScaleSet(ctx, client, cfg, logger)
	if err != nil {
		return err
	}
	client.SetSystemInfo(cfg.systemInfo(scaleSet.ID))
	if cfg.DeleteOnExit {
		shutdown.add(deleteScaleSetStep(func(ctx context.Context) error {
			logger.Info("Deleting scale set", slog.Int("scaleSetID", scaleSet.ID))
			return client.DeleteRunnerScaleSet(ctx, scaleSet.ID)
		}))
	}

	owner, err := os.Hostname()
	if err != nil {
		owner = uuid.NewString()
	}
	session, err := openSession(ctx, logger, 15*time.Second, 2*time.Minute, func() (*scaleset.MessageSessionClient, error) {
		return client.MessageSessionClient(ctx, scaleSet.ID, owner)
	})
	if err != nil {
		return fmt.Errorf("create message session: %w", err)
	}
	shutdown.add(closeSessionStep(session.Close))

	// Only now clean up: the local lock rules out another daemon on this host,
	// and GitHub has just granted us the scale set's only message session.
	if err := prov.CleanupOrphans(ctx); err != nil {
		return fmt.Errorf("clean up orphaned VMs: %w", err)
	}

	s := scaler.New(ctx, scaler.Config{
		ScaleSetID:   scaleSet.ID,
		NamePrefix:   namePrefix,
		MinRunners:   cfg.MinRunners,
		MaxRunners:   cfg.MaxRunners,
		StartTimeout: cfg.StartTimeout,
	}, client, provisioner{prov}, logger.WithGroup("scaler"))
	shutdown.add(stopRunnersStep(s.Shutdown))

	// Periodic refresh starts only now, after orphan cleanup: a refresh VM
	// created earlier would carry this daemon's owner label and be destroyed
	// by that cleanup.
	if cfg.ImageRefreshInterval > 0 && !strings.Contains(cfg.RunnerImage, "@") {
		w := startRefreshWorker(ctx, cfg.ImageRefreshInterval, func(ctx context.Context) error {
			return refreshImage(ctx, prov, namePrefix, logger)
		}, logger, prov.ImageRef)
		shutdown.add(stopRefreshStep(w.stop))
	}

	l, err := listener.New(lifetimeClient{Client: session, life: ctx, timeout: 30 * time.Second}, listener.Config{
		ScaleSetID: scaleSet.ID,
		MaxRunners: cfg.MaxRunners,
		Logger:     logger.WithGroup("listener"),
	})
	if err != nil {
		return fmt.Errorf("create listener: %w", err)
	}

	logger.Info("Listening for jobs",
		slog.String("scaleSet", scaleSet.Name),
		slog.Int("scaleSetID", scaleSet.ID),
		slog.Any("labels", labelNames(scaleSet.Labels)),
	)
	if err := l.Run(ctx, s); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("listener: %w", err)
	}
	return nil
}

// refreshImage pulls the configured image and moves new runners to it.
func refreshImage(ctx context.Context, prov *microvm.Provisioner, namePrefix string, logger *slog.Logger) error {
	start := time.Now()
	res, err := prov.Refresh(ctx, namePrefix+"-refresh")
	if err != nil {
		return err
	}
	version := res.RunnerVersion
	if version == "" {
		version = "unknown"
	}
	attrs := []any{slog.String("image", res.Ref), slog.String("runnerVersion", version), slog.Duration("took", time.Since(start))}
	switch {
	case res.Previous == "":
		logger.Info("Runner image pinned", attrs...)
	case res.Changed:
		logger.Info("Runner image updated; new runners use it", append(attrs, slog.String("previous", res.Previous))...)
	default:
		logger.Info("Runner image unchanged", attrs...)
	}
	return nil
}

// ensureScaleSet reuses an existing scale set with the configured name, so the
// daemon can restart without losing its registration, or creates a new one.
func ensureScaleSet(ctx context.Context, client *scaleset.Client, cfg Config, logger *slog.Logger) (*scaleset.RunnerScaleSet, error) {
	groupID := 1
	if cfg.RunnerGroup != scaleset.DefaultRunnerGroup {
		g, err := client.GetRunnerGroupByName(ctx, cfg.RunnerGroup)
		if err != nil {
			return nil, fmt.Errorf("look up runner group %q: %w", cfg.RunnerGroup, err)
		}
		groupID = g.ID
	}

	desired := &scaleset.RunnerScaleSet{
		Name:          cfg.ScaleSetName,
		RunnerGroupID: groupID,
		Labels:        cfg.ScaleSetLabels(),
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	}

	existing, err := client.GetRunnerScaleSet(ctx, groupID, cfg.ScaleSetName)
	if err != nil {
		return nil, fmt.Errorf("look up scale set: %w", err)
	}
	if existing == nil {
		ss, err := client.CreateRunnerScaleSet(ctx, desired)
		if err != nil {
			return nil, fmt.Errorf("create scale set: %w", err)
		}
		logger.Info("Created scale set", slog.Int("scaleSetID", ss.ID))
		return ss, nil
	}

	ss, err := client.UpdateRunnerScaleSet(ctx, existing.ID, desired)
	if err != nil {
		return nil, fmt.Errorf("update scale set %d: %w", existing.ID, err)
	}
	logger.Info("Reusing existing scale set", slog.Int("scaleSetID", ss.ID))
	return ss, nil
}

func labelNames(labels []scaleset.Label) []string {
	out := make([]string, len(labels))
	for i, l := range labels {
		out[i] = l.Name
	}
	return out
}

// provisioner adapts *microvm.Provisioner to scaler.Provisioner.
type provisioner struct{ *microvm.Provisioner }

func (p provisioner) Start(ctx context.Context, name, jitConfig string) (scaler.RunnerVM, error) {
	vm, err := p.Provisioner.Start(ctx, name, jitConfig)
	if vm == nil {
		return nil, err // avoid a non-nil interface holding a nil *VM
	}
	return vm, err // a VM returned with an error still exists
}
