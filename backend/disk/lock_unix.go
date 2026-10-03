//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package disk

import (
	"fmt"
	"os"
	"syscall"
)

func lockRoot(root *os.Root) (*os.File, error) {
	f, err := root.OpenFile(".gateway/LOCK", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("disk root is already in use by another gateway: %w", err)
	}
	return f, nil
}
