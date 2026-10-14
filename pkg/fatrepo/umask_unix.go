//go:build unix

package fatrepo

import (
	"io/fs"
	"syscall"
)

// umask returns the process umask. Go has no getter, so like Python it sets
// the umask to 0 and restores it. That briefly changes a process-wide value,
// which is a race only if another goroutine creates files meanwhile; the
// filter is single-threaded at this point.
func umask() fs.FileMode {
	old := syscall.Umask(0)
	syscall.Umask(old)
	return fs.FileMode(old)
}
