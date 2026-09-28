package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestValidateRegistrationURL(t *testing.T) {
	valid := []string{
		"https://github.com/org",
		"https://github.com/org/",
		"https://github.com/org/repo",
		"https://github.com/enterprises/acme",
		"https://ghes.example.com/org/repo",
		"https://ghes.example.com:8443/org",
	}
	for _, u := range valid {
		if err := validateRegistrationURL(u); err != nil {
			t.Errorf("%s: unexpected error: %v", u, err)
		}
	}

	invalid := map[string]string{
		"http://github.com/org":            "https",
		"HTTP://github.com/org":            "https",
		"ftp://github.com/org":             "https",
		"github.com/org":                   "https",
		"/org/repo":                        "https",
		"https:///org":                     "host",
		"https://:443/org":                 "host",
		"https://user:pass@github.com/org": "credentials",
		"https://ghp_token@github.com/org": "credentials",
		"https://github.com/org?x=1":       "query",
		"https://github.com/org?":          "query",
		"https://github.com/org#frag":      "fragment",
		"https://github.com":               "path",
		"https://github.com/":              "path",
		"https://github.com/a/b/c":         "path",
		"https://github.com/org//repo":     "path",
		"https://github.com/%zz":           "invalid URL escape",
	}
	for u, want := range invalid {
		err := validateRegistrationURL(u)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want error containing %q, got %v", u, want, err)
		}
	}
}

// Validation runs in PreRunE, so an invalid config must fail before RunE
// (runtime setup, VM cleanup, any GitHub request) can start.
func TestInvalidConfigFailsBeforeRun(t *testing.T) {
	base := []string{"--name", "msb", "--token", "ghp_synthetic"}
	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		envFile string
		wantErr string
	}{
		{name: "http url", args: []string{"--url", "http://github.com/org"}, wantErr: "https"},
		{name: "negative duration flag", args: []string{"--url", "https://github.com/org", "--max-job-duration", "-1h"}, wantErr: "must not be negative"},
		{name: "negative duration env", args: []string{"--url", "https://github.com/org"}, env: map[string]string{"WILLET_MAX_JOB_DURATION": "-30m"}, wantErr: "must not be negative"},
		{name: "registry username without password", args: []string{"--url", "https://github.com/org", "--registry-username", "bot"}, wantErr: "must be set together"},
		{name: "registry password without username", args: []string{"--url", "https://github.com/org"}, envFile: "", env: map[string]string{"WILLET_REGISTRY_PASSWORD_FILE": "PASSWORD_FILE"}, wantErr: "must be set together"},
		{name: "empty registry password file", args: []string{"--url", "https://github.com/org", "--registry-username", "bot"}, env: map[string]string{"WILLET_REGISTRY_PASSWORD_FILE": "EMPTY_FILE"}, wantErr: "must be set together"},
		{name: "missing registry password file", args: []string{"--url", "https://github.com/org", "--registry-username", "bot", "--registry-password-file", "/nonexistent/pw"}, wantErr: "read --registry-password-file"},
		{name: "hostname in network allow", args: []string{"--url", "https://github.com/org", "--network-allow", "10.0.0.1,ghes.corp"}, wantErr: "--network-allow"},
		{name: "registry mirror without docker", args: []string{"--url", "https://github.com/org", "--docker=false", "--docker-registry-mirror", "http://host.microsandbox.internal:5000"}, wantErr: "needs --docker"},
		{name: "registry mirror with a path", args: []string{"--url", "https://github.com/org", "--docker-registry-mirror", "http://host.microsandbox.internal:5000/v2"}, wantErr: "--docker-registry-mirror"},
		{name: "network allow empty port", args: []string{"--url", "https://github.com/org"}, envFile: "WILLET_NETWORK_ALLOW=10.0.5.10:${UNSET_PORT}\n", wantErr: "invalid port"},
		{name: "network allow everything", args: []string{"--url", "https://github.com/org"}, env: map[string]string{"WILLET_NETWORK_ALLOW": "0.0.0.0/0"}, wantErr: "every address"},
		{name: "unknown log level", args: []string{"--url", "https://github.com/org", "--log-level", "verbose"}, wantErr: "--log-level"},
		{name: "unknown log format", args: []string{"--url", "https://github.com/org"}, env: map[string]string{"WILLET_LOG_FORMAT": "jsno"}, wantErr: "--log-format"},
		{name: "docker disk too small", args: []string{"--url", "https://github.com/org", "--docker", "--docker-disk", "100"}, wantErr: "--docker-disk"},
		{name: "negative refresh interval", args: []string{"--url", "https://github.com/org", "--image-refresh-interval", "-1h"}, wantErr: "--image-refresh-interval"},
		{name: "too frequent refresh", args: []string{"--url", "https://github.com/org", "--image-refresh-interval", "30s"}, wantErr: "at least 1m"},
		{name: "zero start timeout", args: []string{"--url", "https://github.com/org", "--start-timeout", "0"}, wantErr: "--start-timeout must be positive"},
		{name: "negative duration env file", args: []string{"--url", "https://github.com/org"}, envFile: "WILLET_MAX_JOB_DURATION=-5s\n", wantErr: "must not be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				switch v {
				case "PASSWORD_FILE":
					v = writeFile(t, "s3cret\n")
				case "EMPTY_FILE":
					v = writeFile(t, "\n")
				}
				t.Setenv(k, v)
			}
			args := append(append([]string{}, base...), tt.args...)
			if tt.envFile != "" {
				args = append(args, "--env-file", writeFile(t, tt.envFile))
			}

			cmd := newCommand()
			ran := false
			cmd.RunE = func(*cobra.Command, []string) error { ran = true; return nil }
			cmd.SetArgs(args)
			err := cmd.Execute()
			if ran {
				t.Fatal("RunE ran despite invalid configuration")
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestValidDurationsAccepted(t *testing.T) {
	for _, d := range []string{"0", "0s", "90m", "6h"} {
		cmd := newCommand()
		ran := false
		cmd.RunE = func(*cobra.Command, []string) error { ran = true; return nil }
		cmd.SetArgs([]string{"--url", "https://github.com/org", "--name", "msb", "--token", "ghp_synthetic", "--max-job-duration", d})
		if err := cmd.Execute(); err != nil || !ran {
			t.Errorf("%s: err=%v ran=%v", d, err, ran)
		}
	}
}

func TestReadPasswordFile(t *testing.T) {
	for content, want := range map[string]string{
		"s3cret":        "s3cret",
		"s3cret\n":      "s3cret",
		"s3cret\r\n":    "s3cret",
		" spaced pw \n": " spaced pw ",
	} {
		got, err := readPasswordFile(writeFile(t, content))
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", content, got, err, want)
		}
	}
}

func TestServerAddress(t *testing.T) {
	for u, want := range map[string]string{
		"https://github.com/org":             "github.com:443",
		"https://ghes.corp.example/org/repo": "ghes.corp.example:443",
		"https://ghes.corp.example:8443/org": "ghes.corp.example:8443",
	} {
		c := Config{RegistrationURL: u}
		h, p := c.serverAddress()
		if h+":"+p != want {
			t.Errorf("%s: got %s:%s, want %s", u, h, p, want)
		}
	}
}

func TestValidLoggingSettings(t *testing.T) {
	for _, c := range []struct{ level, format string }{
		{"debug", "text"}, {"INFO", "json"}, {"Warn", "JSON"}, {"error", "text"},
	} {
		cmd := newCommand()
		ran := false
		cmd.RunE = func(*cobra.Command, []string) error { ran = true; return nil }
		cmd.SetArgs([]string{"--url", "https://github.com/org", "--name", "msb", "--token", "ghp_synthetic", "--log-level", c.level, "--log-format", c.format})
		if err := cmd.Execute(); err != nil || !ran {
			t.Errorf("%s/%s: err=%v ran=%v", c.level, c.format, err, ran)
		}
	}
}
