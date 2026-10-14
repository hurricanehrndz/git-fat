package fatrepo

import "os"

// replaceFile renames src over dst. Windows refuses to replace a read-only
// file, as cached objects and some working files are, so dst is made
// writable first and made read-only again if the rename still fails.
func replaceFile(src, dst string) error {
	fi, err := os.Lstat(dst)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o200 != 0 {
		return os.Rename(src, dst)
	}
	if err = os.Chmod(dst, 0o666); err != nil {
		return err
	}
	if err = os.Rename(src, dst); err != nil {
		_ = os.Chmod(dst, fi.Mode().Perm())
		return err
	}
	return nil
}
