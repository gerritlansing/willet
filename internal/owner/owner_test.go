package owner

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func mustID(t *testing.T, u, group, name string) string {
	t.Helper()
	id, err := ID(u, group, name)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestIDDistinguishesScaleSets(t *testing.T) {
	base := mustID(t, "https://github.com/org-a", "default", "msb")
	different := map[string]string{
		"other org":         mustID(t, "https://github.com/org-b", "default", "msb"),
		"repo in same org":  mustID(t, "https://github.com/org-a/repo", "default", "msb"),
		"enterprise":        mustID(t, "https://github.com/enterprises/org-a", "default", "msb"),
		"GHES host":         mustID(t, "https://ghes.example.com/org-a", "default", "msb"),
		"runner group":      mustID(t, "https://github.com/org-a", "ci", "msb"),
		"scale set name":    mustID(t, "https://github.com/org-a", "default", "msb2"),
		"GHES non-443 port": mustID(t, "https://github.com:8443/org-a", "default", "msb"),
		"field boundary":    mustID(t, "https://github.com/org-a", "defaultm", "sb"),
	}
	for name, id := range different {
		if id == base {
			t.Errorf("%s: got the same ID %s", name, id)
		}
	}
	if len(base) != 12 {
		t.Errorf("ID %q should be 12 hex characters", base)
	}
}

func TestIDNormalizesEquivalentSpellings(t *testing.T) {
	base := mustID(t, "https://github.com/Org-A", "default", "msb")
	same := []string{
		mustID(t, "https://github.com/org-a", "default", "msb"),
		mustID(t, "https://github.com/org-a/", "default", "msb"),
		mustID(t, "https://GitHub.com/org-a", "default", "msb"),
		mustID(t, "https://github.com:443/org-a", "default", "msb"),
		mustID(t, " https://github.com/org-a ", " Default ", " MSB "),
	}
	for i, id := range same {
		if id != base {
			t.Errorf("spelling %d: got %s, want %s", i, id, base)
		}
	}
}

func TestAcquireIsExclusive(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir, "abc")
	if err != nil {
		t.Fatal(err)
	}

	_, err = Acquire(dir, "abc")
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second acquire: want ErrLocked, got %v", err)
	}
	if !strings.Contains(err.Error(), "pid "+strconv.Itoa(os.Getpid())) {
		t.Errorf("error should name the holder's pid: %v", err)
	}

	// A different identity is independent.
	other, err := Acquire(dir, "def")
	if err != nil {
		t.Fatalf("other identity: %v", err)
	}
	_ = other.Release()

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("second release: %v", err)
	}
	again, err := Acquire(dir, "abc")
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	_ = again.Release()
}

// A daemon killed without any chance to clean up must not block its successor.
func TestLockRecoveredAfterHolderIsKilled(t *testing.T) {
	if dir := os.Getenv("OWNER_TEST_HOLD_LOCK"); dir != "" {
		// Child process: take the lock, report, then wait to be killed.
		if _, err := Acquire(dir, "crash"); err != nil {
			os.Exit(2)
		}
		os.Stdout.WriteString("locked\n")
		select {}
	}

	dir := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestLockRecoveredAfterHolderIsKilled$")
	child.Env = append(os.Environ(), "OWNER_TEST_HOLD_LOCK="+dir)
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if line, _ := bufio.NewReader(out).ReadString('\n'); line != "locked\n" {
		_ = child.Process.Kill()
		t.Fatalf("child did not take the lock: %q", line)
	}

	if _, err := Acquire(dir, "crash"); !errors.Is(err, ErrLocked) {
		t.Fatalf("while child holds lock: want ErrLocked, got %v", err)
	}

	_ = child.Process.Kill() // SIGKILL: no deferred cleanup runs
	_ = child.Wait()

	l, err := Acquire(dir, "crash")
	if err != nil {
		t.Fatalf("after holder was killed: %v", err)
	}
	_ = l.Release()
}

func TestStateDir(t *testing.T) {
	t.Setenv("STATE_DIRECTORY", "/var/lib/willet:/var/lib/other")
	if d, _ := StateDir(); d != "/var/lib/willet" {
		t.Errorf("systemd: got %s", d)
	}
	t.Setenv("STATE_DIRECTORY", "")
	t.Setenv("XDG_STATE_HOME", "/xdg")
	if d, _ := StateDir(); d != "/xdg/willet" {
		t.Errorf("xdg: got %s", d)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/u")
	if d, _ := StateDir(); d != "/home/u/.local/state/willet" {
		t.Errorf("default: got %s", d)
	}
}
