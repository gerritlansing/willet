package owner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrLocked means another live process holds the lock for this identity.
var ErrLocked = errors.New("another willet is already running for this scale set")

// Lock is held for as long as the daemon runs. The kernel releases it when
// the process exits for any reason, including a crash or SIGKILL, so a new
// daemon can always take over from a dead one.
type Lock struct {
	f *os.File
}

// StateDir returns where lock files live: $STATE_DIRECTORY (set by systemd's
// StateDirectory=), else $XDG_STATE_HOME/willet, else
// ~/.local/state/willet.
func StateDir() (string, error) {
	if d := os.Getenv("STATE_DIRECTORY"); d != "" {
		// systemd may pass several colon-separated directories.
		return strings.Split(d, ":")[0], nil
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "willet"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate state directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "willet"), nil
}

// Acquire takes the exclusive lock for id in dir without blocking. It returns
// an error wrapping ErrLocked, naming the holder's PID, if the lock is taken.
func Acquire(dir, id string) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	path := filepath.Join(dir, id+".lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := tryLock(f); err != nil {
		holder, _ := os.ReadFile(path)
		_ = f.Close()
		if errors.Is(err, errWouldBlock) {
			pid := strings.TrimSpace(string(holder))
			if pid == "" {
				pid = "unknown"
			}
			return nil, fmt.Errorf("%w (pid %s, lock %s)", ErrLocked, pid, path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}

	// Record our PID for the error message a second daemon prints. This is
	// informational only; the flock is what provides exclusion.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &Lock{f: f}, nil
}

// Release gives up the lock. It is safe to call more than once.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
