package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// fakeSystemctl puts a systemctl on PATH that reports the given ActiveState
// and records every other call. It returns a function listing the calls.
func fakeSystemctl(t *testing.T, state string) func() []string {
	t.Helper()
	dir := t.TempDir()
	logPath, statePath := filepath.Join(dir, "log"), filepath.Join(dir, "state")
	if err := os.WriteFile(statePath, []byte(state+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
case "$*" in
  *ActiveState*) cat "` + statePath + `" ;;
  *KillMode*) printf 'KillMode=mixed\nTimeoutStopUSec=infinity\n' ;;
  *) echo "$*" >> "` + logPath + `" ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		b, _ := os.ReadFile(logPath)
		return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(b)), "\n", ";"))
	}
}

func calls(t *testing.T, get func() []string) []string {
	t.Helper()
	var out []string
	for _, c := range strings.Split(strings.Join(get(), " "), ";") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

func TestTerminateInterruptsDrainingUnit(t *testing.T) {
	get := fakeSystemctl(t, "deactivating")
	cmd := newTerminateCommand()
	cmd.SetContext(context.Background())
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"stop --no-block willet", "kill --kill-whom=main --signal=SIGINT willet", "stop willet"}
	if got := calls(t, get); !slices.Equal(got, want) {
		t.Fatalf("systemctl calls = %q, want %q", got, want)
	}
}

func TestStopAndTerminateLeaveInactiveUnitAlone(t *testing.T) {
	for _, state := range []string{"inactive", "failed"} {
		for _, newCmd := range []func() *cobra.Command{newStopCommand, newTerminateCommand} {
			get := fakeSystemctl(t, state)
			c := newCmd()
			c.SetContext(context.Background())
			var out bytes.Buffer
			c.SetErr(&out)
			if err := c.RunE(c, nil); err != nil {
				t.Fatal(err)
			}
			if got := calls(t, get); len(got) != 0 {
				t.Fatalf("%s: systemctl called: %q", state, got)
			}
			if !strings.Contains(out.String(), "not running ("+state+")") {
				t.Fatalf("output = %q", out.String())
			}
		}
	}
}

// A forced restart must finish stopping the old daemon before it starts the
// new one, so the second signal can only reach the old daemon.
func TestForcedRestartStopsBeforeStarting(t *testing.T) {
	get := fakeSystemctl(t, "active")
	u := unit{name: "willet", out: &bytes.Buffer{}}
	if err := u.restart(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	want := []string{"stop --no-block willet", "kill --kill-whom=main --signal=SIGINT willet", "stop willet", "start willet"}
	if got := calls(t, get); !slices.Equal(got, want) {
		t.Fatalf("systemctl calls = %q, want %q", got, want)
	}
}

func TestDrainingRestartIsOneBlockingCall(t *testing.T) {
	get := fakeSystemctl(t, "active")
	u := unit{name: "willet", out: &bytes.Buffer{}}
	if err := u.restart(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if got := calls(t, get); !slices.Equal(got, []string{"restart willet"}) {
		t.Fatalf("systemctl calls = %q", got)
	}
}
