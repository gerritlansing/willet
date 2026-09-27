//go:build unix

package owner

import (
	"errors"
	"os"
	"syscall"
)

var errWouldBlock = syscall.EWOULDBLOCK

func tryLock(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
