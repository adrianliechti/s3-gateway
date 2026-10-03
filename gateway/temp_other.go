//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package gateway

import "os"

func stagingFile(dir, pattern string) (*os.File, error) {
	return os.CreateTemp(dir, pattern)
}
