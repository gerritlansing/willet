package microvm

import (
	"context"
	"strings"
	"testing"
	"time"
)

func validConfig() Config {
	return Config{
		Image:        "ghcr.io/actions/actions-runner:latest",
		CPUs:         2,
		MemoryMiB:    4096,
		User:         "runner",
		RunnerDir:    "/home/runner",
		OwnerID:      "0123456789ab",
		ScaleSetName: "msb",
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"valid", func(*Config) {}, ""},
		{"zero duration means unlimited", func(c *Config) { c.MaxDuration = 0 }, ""},
		{"positive duration", func(c *Config) { c.MaxDuration = time.Hour }, ""},
		{"negative duration", func(c *Config) { c.MaxDuration = -time.Hour }, "must not be negative"},
		{"no image", func(c *Config) { c.Image = "" }, "image"},
		{"no cpus", func(c *Config) { c.CPUs = 0 }, "positive"},
		{"no memory", func(c *Config) { c.MemoryMiB = 0 }, "positive"},
		{"no user", func(c *Config) { c.User = "" }, "user"},
		{"relative runner dir", func(c *Config) { c.RunnerDir = "home/runner" }, "absolute"},
		{"registry username without password", func(c *Config) { c.RegistryUsername = "bot" }, "set together"},
		{"registry password without username", func(c *Config) { c.RegistryPassword = "pw" }, "set together"},
		{"registry credentials", func(c *Config) { c.RegistryUsername, c.RegistryPassword = "bot", "pw" }, ""},
		{"no owner ID", func(c *Config) { c.OwnerID = "" }, "owner"},
		{"no scale set name", func(c *Config) { c.ScaleSetName = "" }, "scale set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mutate(&c)
			err := c.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestNewRejectsInvalidConfigBeforeRuntimeSetup(t *testing.T) {
	c := validConfig()
	c.MaxDuration = -time.Minute
	// The context is already canceled: if New reached runtime setup it would
	// fail with a context error rather than the validation error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New(ctx, c, nil)
	if err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("want validation error, got %v", err)
	}
}
