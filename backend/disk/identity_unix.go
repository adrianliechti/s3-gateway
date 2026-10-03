//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package disk

import (
	"fmt"
	"io/fs"
	"syscall"
)

// The inode distinguishes atomic replacements even when size and mtime are
// preserved. All fields survive the staging-to-object rename; ctime does not.
// Atomic publication requires the same filesystem. Its device number is not
// included because it can change when that filesystem is remounted.
func fingerprint(i fs.FileInfo) string {
	st := i.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%x-%x-%x", st.Ino, i.ModTime().UnixNano(), i.Size())
}
