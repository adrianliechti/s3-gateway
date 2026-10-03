//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package gateway

import "os"

// Unlinked staging remains accessible through its open descriptor and is
// reclaimed by the OS even when the process exits without running defers.
func stagingFile(dir, pattern string) (*os.File, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	if err = os.Remove(f.Name()); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
