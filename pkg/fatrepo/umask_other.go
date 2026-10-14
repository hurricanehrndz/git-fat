//go:build !unix

package fatrepo

import "io/fs"

// umask returns 0: Windows has no umask, and os.Chmod there only toggles the
// read-only attribute.
func umask() fs.FileMode { return 0 }
