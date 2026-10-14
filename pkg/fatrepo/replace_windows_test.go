package fatrepo

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReplaceFileReadOnly replaces a read-only file, which a plain rename
// cannot on Windows, and leaves it read-only when the rename fails.
func TestReplaceFileReadOnly(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	t.Cleanup(func() { _ = os.Chmod(dst, 0o666) })
	if err := os.WriteFile(dst, []byte("old"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("new"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "new" {
		t.Fatalf("dst = %q, %v; want new", b, err)
	}
	if err := replaceFile(filepath.Join(dir, "missing"), dst); err == nil {
		t.Fatal("replaceFile from a missing file succeeded")
	}
	if fi, err := os.Stat(dst); err != nil || fi.Mode().Perm()&0o200 != 0 {
		t.Fatalf("dst after failed rename: %v, %v; want read-only", fi.Mode(), err)
	}
}
