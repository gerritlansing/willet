//go:build integration

package microvm

import (
	"log/slog"
	"testing"

	msb "github.com/superradcompany/microsandbox/sdk/go"
)

// An older runtime left in the runtime home by a previous willet is replaced
// by the one this SDK needs.
func TestAlignRuntimeUpdatesOlderHome(t *testing.T) {
	config := msb.RuntimeConfig{Home: t.TempDir()}
	old, err := msb.InstallRuntime(t.Context(), config, msb.InstallOptions{Version: "0.7.3"})
	if err != nil {
		t.Fatalf("install old runtime: %v", err)
	}
	if v, err := msbVersion(t.Context(), old.MSBPath); err != nil || v != "0.7.3" {
		t.Fatalf("old runtime version %q, %v", v, err)
	}

	rt, err := alignRuntime(t.Context(), slog.New(slog.DiscardHandler), msb.SDKVersion(), sdkRuntime(config))
	if err != nil {
		t.Fatalf("align: %v", err)
	}
	if rt.MSBPath != old.MSBPath {
		t.Errorf("runtime moved from %s to %s", old.MSBPath, rt.MSBPath)
	}
	if v, err := msbVersion(t.Context(), rt.MSBPath); err != nil || v != msb.SDKVersion() {
		t.Fatalf("aligned runtime version %q, %v; want %s", v, err, msb.SDKVersion())
	}
}
