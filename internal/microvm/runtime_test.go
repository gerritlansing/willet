package microvm

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	msb "github.com/superradcompany/microsandbox/sdk/go"
)

// fakeRuntime simulates an installed runtime whose version changes when it
// is reinstalled.
type fakeRuntime struct {
	origin     msb.RuntimeOrigin
	version    string
	installs   string // version an install leaves behind
	installErr error

	installed int
}

func (f *fakeRuntime) setup() runtimeSetup {
	return runtimeSetup{
		ensure: func(context.Context) (msb.ResolvedRuntime, error) {
			return msb.ResolvedRuntime{MSBPath: "/home/willet/.microsandbox/bin/msb", Origin: f.origin}, nil
		},
		install: func(context.Context) (msb.ResolvedRuntime, error) {
			f.installed++
			if f.installErr != nil {
				return msb.ResolvedRuntime{}, f.installErr
			}
			f.version = f.installs
			return msb.ResolvedRuntime{MSBPath: "/home/willet/.microsandbox/bin/msb", Origin: msb.RuntimeOriginInstalled}, nil
		},
		version: func(context.Context, string) (string, error) { return f.version, nil },
	}
}

func TestAlignRuntime(t *testing.T) {
	cases := []struct {
		name          string
		rt            fakeRuntime
		wantInstalled int
		wantErr       string
	}{
		{name: "matching", rt: fakeRuntime{origin: msb.RuntimeOriginHome, version: "0.7.7"}},
		{name: "older in home is updated", rt: fakeRuntime{origin: msb.RuntimeOriginHome, version: "0.7.3", installs: "0.7.7"}, wantInstalled: 1},
		{name: "newer is never downgraded", rt: fakeRuntime{origin: msb.RuntimeOriginHome, version: "0.8.0"}, wantErr: "upgrade willet"},
		{name: "unparseable is left alone", rt: fakeRuntime{origin: msb.RuntimeOriginHome, version: "dev"}, wantErr: "upgrade willet"},
		{name: "explicit runtime is left alone", rt: fakeRuntime{origin: msb.RuntimeOriginEnvironment, version: "0.7.3"}, wantErr: "selected by environment"},
		{name: "install failure", rt: fakeRuntime{origin: msb.RuntimeOriginHome, version: "0.7.3", installErr: errors.New("download failed")}, wantInstalled: 1, wantErr: "download failed"},
		{name: "install leaves wrong version", rt: fakeRuntime{origin: msb.RuntimeOriginHome, version: "0.7.3", installs: "0.7.6"}, wantInstalled: 1, wantErr: "reports version 0.7.6"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := alignRuntime(context.Background(), slog.New(slog.DiscardHandler), "0.7.7", c.rt.setup())
			if c.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
			if c.rt.installed != c.wantInstalled {
				t.Fatalf("installed %d times, want %d", c.rt.installed, c.wantInstalled)
			}
		})
	}
}

func TestMsbVersion(t *testing.T) {
	cases := map[string]string{
		"msb 0.7.7\n": "0.7.7",
		"msb\n":       "",
		"0.7.7\n":     "",
	}
	for out, want := range cases {
		path := filepath.Join(t.TempDir(), "msb")
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '"+out+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := msbVersion(context.Background(), path)
		if got != want || (want == "") != (err != nil) {
			t.Errorf("output %q: got %q, %v; want %q", out, got, err, want)
		}
	}
}
