package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

func testFlags() (*pflag.FlagSet, *string, *int, *string) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.String("env-file", "", "")
	name := fs.String("name", "", "")
	maxRunners := fs.Int("max-runners", 4, "")
	level := fs.String("log-level", "info", "")
	return fs, name, maxRunners, level
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadEnvPrecedence(t *testing.T) {
	path := writeFile(t, `
# comment
WILLET_NAME=from-file
WILLET_MAX_RUNNERS=7
WILLET_LOG_LEVEL=warn
UNRELATED=ignored
`)
	fs, name, maxRunners, level := testFlags()
	if err := fs.Parse([]string{"--env-file", path, "--log-level", "debug"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WILLET_MAX_RUNNERS", "9")

	if err := loadEnv(fs, path); err != nil {
		t.Fatal(err)
	}
	if *name != "from-file" {
		t.Errorf("name = %q, want value from env file", *name)
	}
	if *maxRunners != 9 {
		t.Errorf("max-runners = %d, want process env to beat env file", *maxRunners)
	}
	if *level != "debug" {
		t.Errorf("log-level = %q, want flag to beat env file", *level)
	}
}

func TestLoadEnvFileFromEnvironment(t *testing.T) {
	path := writeFile(t, "WILLET_NAME=via-env-var\n")
	t.Setenv(envFileVar, path)
	fs, name, _, _ := testFlags()
	_ = fs.Parse(nil)

	if err := loadEnv(fs, ""); err != nil {
		t.Fatal(err)
	}
	if *name != "via-env-var" {
		t.Errorf("name = %q", *name)
	}
}

func TestLoadEnvRejectsUnknownSettings(t *testing.T) {
	path := writeFile(t, "WILLET_MAX_RUNNER=3\nWILLET_NAME=x\n")
	fs, _, _, _ := testFlags()
	_ = fs.Parse(nil)

	err := loadEnv(fs, path)
	if err == nil || !strings.Contains(err.Error(), "WILLET_MAX_RUNNER") {
		t.Fatalf("want error naming the typo, got %v", err)
	}
}

func TestLoadEnvRejectsNestedEnvFile(t *testing.T) {
	path := writeFile(t, "WILLET_ENV_FILE=/other.env\n")
	fs, _, _, _ := testFlags()
	_ = fs.Parse(nil)

	if err := loadEnv(fs, path); err == nil {
		t.Fatal("want error for WILLET_ENV_FILE inside an env file")
	}
}

func TestLoadEnvBadValue(t *testing.T) {
	path := writeFile(t, "WILLET_MAX_RUNNERS=lots\n")
	fs, _, _, _ := testFlags()
	_ = fs.Parse(nil)

	err := loadEnv(fs, path)
	if err == nil || !strings.Contains(err.Error(), "WILLET_MAX_RUNNERS") {
		t.Fatalf("want error naming the variable, got %v", err)
	}
}

func TestLoadEnvMissingFile(t *testing.T) {
	fs, _, _, _ := testFlags()
	_ = fs.Parse(nil)
	if err := loadEnv(fs, filepath.Join(t.TempDir(), "missing.env")); err == nil {
		t.Fatal("want error for a missing env file")
	}
}

// The shipped example must stay loadable, with every commented-out default
// enabled, against the real flag set.
func TestExampleEnvFileIsValid(t *testing.T) {
	raw, err := os.ReadFile("../../willet.env.example")
	if err != nil {
		t.Fatal(err)
	}
	uncommented := strings.ReplaceAll(string(raw), "\n#WILLET_", "\nWILLET_")
	path := writeFile(t, uncommented)

	cmd := newCommand()
	fs := cmd.Flags()
	if err := fs.Parse([]string{"--env-file", path}); err != nil {
		t.Fatal(err)
	}
	for _, k := range os.Environ() {
		if strings.HasPrefix(k, envPrefix) {
			t.Skip("WILLET_* set in the test environment")
		}
	}
	if err := loadEnv(fs, path); err != nil {
		t.Fatal(err)
	}
}
