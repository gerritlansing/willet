package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/actions/scaleset"

	"github.com/gerritlansing/willet/internal/microvm"
)

// Config is the full configuration of the scale set daemon.
type Config struct {
	RegistrationURL string
	ScaleSetName    string
	Labels          []string
	RunnerGroup     string
	MinRunners      int
	MaxRunners      int

	GitHubApp scaleset.GitHubAppAuth
	Token     string

	RunnerImage    string
	CPUs           uint8
	MemoryMiB      uint32
	DiskMiB        uint32
	MaxJobDuration time.Duration
	StartTimeout   time.Duration
	RunnerUser     string
	RunnerDir      string
	SkipWarmup     bool

	ImageRefreshInterval time.Duration

	// Credentials for pulling RunnerImage from a private registry. They are
	// used on the host only and never reach a VM.
	RegistryUsername string
	RegistryPassword string

	// NetworkAllow lists IPs or CIDRs, optionally with ports, that runner VMs
	// may reach in addition to the public internet.
	NetworkAllow []string
	// DNSRebindProtection keeps microsandbox's DNS rebinding protection on.
	DNSRebindProtection bool

	// Docker runs a Docker daemon in each VM on a DockerDiskMiB data disk.
	Docker        bool
	DockerDiskMiB uint32
	// DockerRegistryMirror is a Docker Hub pull-through cache URL for the
	// VMs' Docker daemons; normalized by Validate.
	DockerRegistryMirror string
	networkAllow         []microvm.AllowRule

	DeleteOnExit bool
	LogLevel     string
	LogFormat    string
	// RunnerOutput logs the runners' raw output.
	RunnerOutput bool
}

var invalidNameChars = regexp.MustCompile(`[^a-z0-9-]+`)

// Validate checks the configuration and fills defaults.
func (c *Config) Validate() error {
	if c.RunnerGroup == "" {
		c.RunnerGroup = scaleset.DefaultRunnerGroup
	}
	if err := validateRegistrationURL(c.RegistrationURL); err != nil {
		return fmt.Errorf("invalid --url %q: %w (expected e.g. https://github.com/org, https://github.com/org/repo or https://github.com/enterprises/name)", c.RegistrationURL, err)
	}
	if c.ScaleSetName == "" {
		return errors.New("--name is required")
	}
	if err := c.validateLogging(); err != nil {
		return err
	}
	if c.GitHubApp.Validate() != nil && c.Token == "" {
		return errors.New("credentials required: either --app-client-id, --app-installation-id and --app-private-key, or --token")
	}
	for i, l := range c.Labels {
		if strings.TrimSpace(l) == "" {
			return fmt.Errorf("label %d is empty", i)
		}
	}
	if c.MinRunners < 0 || c.MaxRunners < 1 || c.MaxRunners < c.MinRunners {
		return errors.New("need 0 <= --min-runners <= --max-runners and --max-runners >= 1")
	}
	if c.CPUs == 0 || c.MemoryMiB == 0 {
		return errors.New("--cpus and --memory must be positive")
	}
	if c.ImageRefreshInterval < 0 || (c.ImageRefreshInterval > 0 && c.ImageRefreshInterval < time.Minute) {
		return fmt.Errorf("--image-refresh-interval must be 0 (never) or at least 1m (got %s)", c.ImageRefreshInterval)
	}
	if (c.RegistryUsername == "") != (c.RegistryPassword == "") {
		return errors.New("--registry-username and --registry-password-file must be set together (and the password file must not be empty)")
	}
	rules, err := microvm.ParseAllowRules(c.NetworkAllow)
	if err != nil {
		return fmt.Errorf("--network-allow: %w", err)
	}
	c.networkAllow = rules
	if c.Docker && c.DockerDiskMiB < 1024 {
		return fmt.Errorf("--docker-disk must be at least 1024 MiB (got %d)", c.DockerDiskMiB)
	}
	if c.DockerRegistryMirror != "" {
		if !c.Docker {
			return errors.New("--docker-registry-mirror needs --docker")
		}
		mirror, err := microvm.ParseRegistryMirror(c.DockerRegistryMirror)
		if err != nil {
			return fmt.Errorf("--docker-registry-mirror: %w", err)
		}
		c.DockerRegistryMirror = mirror
	}
	if c.StartTimeout <= 0 {
		return fmt.Errorf("--start-timeout must be positive (got %s)", c.StartTimeout)
	}
	if c.MaxJobDuration < 0 {
		return fmt.Errorf("--max-job-duration must not be negative (got %s); use 0 for no limit", c.MaxJobDuration)
	}
	if c.NamePrefix() == "" {
		return fmt.Errorf("cannot derive a VM name from scale set name %q", c.ScaleSetName)
	}
	return nil
}

// serverAddress returns the host and port runners connect to, taken from the
// registration URL.
func (c *Config) serverAddress() (string, string) {
	u, err := url.Parse(c.RegistrationURL)
	if err != nil {
		return "", ""
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return u.Hostname(), port
}

// readPasswordFile reads a secret from a file, dropping the trailing newline
// most editors and `echo` add.
func readPasswordFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// validateRegistrationURL accepts only an absolute HTTPS URL naming an org,
// repo or enterprise. The client sends management credentials to this host,
// so plain HTTP must never be allowed.
func validateRegistrationURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	switch {
	case u.Scheme != "https":
		return errors.New("scheme must be https")
	case u.Host == "" || u.Hostname() == "":
		return errors.New("host is missing")
	case u.User != nil:
		return errors.New("credentials must not be embedded in the URL")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return errors.New("query and fragment are not allowed")
	}
	// Mirrors the scopes the scaleset client accepts: org, org/repo or
	// enterprises/name.
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) > 2 || slices.Contains(parts, "") {
		return errors.New("path must name an org, repo or enterprise")
	}
	return nil
}

// NamePrefix is a hostname-safe prefix derived from the scale set name.
func (c *Config) NamePrefix() string {
	p := invalidNameChars.ReplaceAllString(strings.ToLower(c.ScaleSetName), "-")
	p = strings.Trim(p, "-")
	if len(p) > 40 {
		p = strings.TrimRight(p[:40], "-")
	}
	return p
}

// ScaleSetLabels returns the labels workflows use in runs-on.
func (c *Config) ScaleSetLabels() []scaleset.Label {
	if len(c.Labels) == 0 {
		return []scaleset.Label{{Name: c.ScaleSetName}}
	}
	out := make([]scaleset.Label, len(c.Labels))
	for i, l := range c.Labels {
		out[i] = scaleset.Label{Name: strings.TrimSpace(l)}
	}
	return out
}

func (c *Config) systemInfo(scaleSetID int) scaleset.SystemInfo {
	return scaleset.SystemInfo{
		System:     "willet",
		Subsystem:  "microsandbox",
		Version:    version,
		CommitSHA:  commit,
		ScaleSetID: scaleSetID,
	}
}

// ScalesetClient builds an API client from whichever credentials were given.
func (c *Config) ScalesetClient() (*scaleset.Client, error) {
	if c.GitHubApp.Validate() == nil {
		return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
			GitHubConfigURL: c.RegistrationURL,
			GitHubAppAuth:   c.GitHubApp,
			SystemInfo:      c.systemInfo(0),
		})
	}
	return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL:     c.RegistrationURL,
		PersonalAccessToken: c.Token,
		SystemInfo:          c.systemInfo(0),
	})
}

var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// validateLogging rejects unknown log settings, so a typo like "jsno" fails
// at startup instead of silently producing output a log collector can't parse.
func (c *Config) validateLogging() error {
	if _, ok := logLevels[strings.ToLower(c.LogLevel)]; !ok {
		return fmt.Errorf("--log-level %q: want debug, info, warn or error", c.LogLevel)
	}
	switch strings.ToLower(c.LogFormat) {
	case "text", "json":
		return nil
	}
	return fmt.Errorf("--log-format %q: want text or json", c.LogFormat)
}

// Logger builds the process logger from validated settings.
func (c *Config) Logger() *slog.Logger {
	opts := &slog.HandlerOptions{Level: logLevels[strings.ToLower(c.LogLevel)]}
	if strings.ToLower(c.LogFormat) == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}
