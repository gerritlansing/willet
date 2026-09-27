//go:build !unix

package owner

import (
	"errors"
	"os"
)

var errWouldBlock = errors.New("would block")

func tryLock(*os.File) error {
	return errors.New("single-instance locking is only implemented on Unix")
}
