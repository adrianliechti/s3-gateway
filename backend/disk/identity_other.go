//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package disk

import (
	"fmt"
	"io/fs"
)

// New rejects platforms without the required filesystem primitives.
func fingerprint(i fs.FileInfo) string {
	return fmt.Sprintf("%x-%x", i.ModTime().UnixNano(), i.Size())
}
