//go:build linux

package lifecycle

import (
	"fmt"
	"os"
	"syscall"
)

// Lock must live on a local filesystem shared by all release containers.
func Lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another instance owns state: %w", err)
	}
	return func() { _ = f.Close() }, nil
}
