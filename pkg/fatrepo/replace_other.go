//go:build !windows

package fatrepo

import "os"

// replaceFile renames src over dst. Elsewhere than on Windows a read-only dst
// is no obstacle.
func replaceFile(src, dst string) error {
	return os.Rename(src, dst)
}
