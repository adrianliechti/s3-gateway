//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package disk

import (
	"fmt"
	"os"
)

func lockRoot(root *os.Root) (*os.File, error) {
	return nil, fmt.Errorf("disk backend requires a platform with advisory file locks")
}
