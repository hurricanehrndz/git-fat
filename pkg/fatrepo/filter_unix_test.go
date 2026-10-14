//go:build unix

package fatrepo_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestCleanObjectMode checks cached objects are read-only, so a stray write
// through a hard link or editor cannot corrupt the cache.
func TestCleanObjectMode(t *testing.T) {
	old := syscall.Umask(0o027)
	defer syscall.Umask(old)
	r, _ := newRepo(t)
	content := []byte("fat content a\n")
	clean(t, r, content)
	info, err := os.Stat(filepath.Join(r.ObjDir, sha1hex(content)))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), fs.FileMode(0o444&^0o027); got != want {
		t.Fatalf("mode = %v, want %v", got, want)
	}
}
