package microvm

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// runReach runs reachScript under /bin/sh with PATH limited to bin, so each
// test decides which tools the "image" has.
func runReach(t *testing.T, bin, host, port string, env ...string) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", reachScript, "reach", host, port)
	cmd.Env = append([]string{"PATH=" + bin}, env...)
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr):
		return exitErr.ExitCode()
	default:
		t.Fatal(err)
		return -1
	}
}

// fakeCurl installs a curl that prints $FAKE_CONNECT (its -w output), records
// its arguments, and exits with $FAKE_RC.
func fakeCurl(t *testing.T) (bin, argsFile string) {
	t.Helper()
	bin = t.TempDir()
	argsFile = filepath.Join(t.TempDir(), "args")
	script := "#!/bin/sh\necho \"$@\" > " + argsFile + "\nprintf '%s' \"$FAKE_CONNECT\"\nexit $FAKE_RC\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsFile
}

func TestReachScriptCurlFallback(t *testing.T) {
	bin, _ := fakeCurl(t)
	cases := []struct {
		rc      int
		connect string
		want    int
		why     string
	}{
		{0, "0.001", reachOK, "connected and finished"},
		{28, "0.000160", reachOK, "connected; telnet mode then waits for --max-time"},
		{28, "0.000000", reachFailed, "timed out before connecting"},
		{7, "0.000000", reachFailed, "connection refused"},
		{6, "0.000000", reachUnresolved, "name did not resolve"},
		{1, "0.000000", reachUnavailable, "curl built without telnet support"},
		{3, "0.000000", reachUnavailable, "malformed URL"},
		{4, "0.000000", reachUnavailable, "feature not built in"},
		{5, "0.000000", reachUnavailable, "proxy did not resolve"},
		{35, "0.000000", reachUnavailable, "any other error"},
	}
	for _, c := range cases {
		got := runReach(t, bin, "10.0.5.10", "443", "FAKE_RC="+strconv.Itoa(c.rc), "FAKE_CONNECT="+c.connect)
		if got != c.want {
			t.Errorf("curl exit %d (%s): script exit %d, want %d", c.rc, c.why, got, c.want)
		}
	}
}

func TestReachScriptIPv6URL(t *testing.T) {
	bin, argsFile := fakeCurl(t)
	runReach(t, bin, "fd00::1", "443", "FAKE_RC=0", "FAKE_CONNECT=0.001")
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "telnet://[fd00::1]:443") {
		t.Fatalf("IPv6 address not bracketed: %s", args)
	}
}

func TestReachScriptNoTools(t *testing.T) {
	if got := runReach(t, t.TempDir(), "10.0.5.10", "443"); got != reachUnavailable {
		t.Fatalf("with no tools: exit %d, want %d (unavailable, not success)", got, reachUnavailable)
	}
}

// With the real curl, a peer that accepts but never replies must count as
// reachable, and the probe must finish within its own time limit.
func TestReachScriptRealCurlSilentPeer(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl not installed")
	}
	bin := t.TempDir()
	if err := os.Symlink(curl, filepath.Join(bin, "curl")); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // hold the connection open, say nothing
		}
	}()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)

	begin := time.Now()
	if got := runReach(t, bin, "127.0.0.1", port); got != reachOK {
		t.Fatalf("silent peer: exit %d, want reachable", got)
	}
	if d := time.Since(begin); d > 20*time.Second {
		t.Fatalf("probe took %s, beyond curl's --max-time", d)
	}

	closed, _ := net.Listen("tcp4", "127.0.0.1:0")
	closedPort := strconv.Itoa(closed.Addr().(*net.TCPAddr).Port)
	closed.Close()
	if got := runReach(t, bin, "127.0.0.1", closedPort); got != reachFailed {
		t.Fatalf("closed port: exit %d, want %d", got, reachFailed)
	}
}
