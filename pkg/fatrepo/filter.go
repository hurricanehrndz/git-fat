package fatrepo

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/hurricanehrndz/git-fat/v2/pkg/fatstub"
)

// blockSize is how much of the input Clean inspects for an existing stub.
const blockSize = 4096

// readUpTo reads up to n bytes from r, returning fewer only at end of input.
func readUpTo(r io.Reader, n int) ([]byte, error) {
	buf := make([]byte, n)
	got, err := io.ReadFull(r, buf)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		err = nil
	}
	return buf[:got], err
}

// Clean is the git clean filter: it caches the content read from r in ObjDir
// under its SHA1 and writes the content's stub to w. Input that already is a
// stub is written through unchanged so a stub is never cleaned twice. Empty
// input is cached like any other content.
func (r *Repo) Clean(in io.Reader, w io.Writer) error {
	first, err := readUpTo(in, blockSize)
	if err != nil {
		return err
	}
	if fatstub.IsStub(first) {
		_, err = w.Write(first)
		return err
	}
	sha, size, err := r.cache(io.MultiReader(bytes.NewReader(first), in))
	if err != nil {
		return err
	}
	_, err = w.Write(fatstub.Encode(sha, size))
	return err
}

// cache streams in into ObjDir/<sha1> via a temp file in ObjDir, returning the
// digest and byte count. The temp file never outlives the call.
func (r *Repo) cache(in io.Reader) (sha string, size int64, err error) {
	tmp, err := os.CreateTemp(r.ObjDir, "tmp-")
	if err != nil {
		return "", 0, err
	}
	defer func() {
		// After a successful rename this fails harmlessly with ErrNotExist.
		_ = os.Remove(tmp.Name())
	}()
	h := sha1.New()
	size, err = io.Copy(io.MultiWriter(tmp, h), in)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	sha = hex.EncodeToString(h.Sum(nil))
	obj := filepath.Join(r.ObjDir, sha)
	if cached(obj, size) {
		r.logf(true, "filter-clean: cache already exists %s", obj)
		return sha, size, nil
	}
	if err = os.Chmod(tmp.Name(), 0o444&^umask()); err != nil {
		return "", 0, err
	}
	// The rename replaces a truncated or otherwise wrong-sized object. It
	// fails, leaving the error to the caller, when obj is a directory.
	if err = replaceFile(tmp.Name(), obj); err != nil {
		// A concurrent clean of the same content may have published first and
		// still hold the object open, which on Windows fails the rename.
		if cached(obj, size) {
			return sha, size, nil
		}
		return "", 0, err
	}
	r.logf(true, "filter-clean: caching to %s", obj)
	return sha, size, nil
}

// cached reports whether obj is a regular file of the given size. It does not
// rehash the content: objects are read-only and named by their digest, and git
// cleans every stat-dirty file, so rehashing would read each file twice. A
// same-size corruption therefore goes unnoticed here; catching it needs a
// separate verify command that rehashes the store.
func cached(obj string, size int64) bool {
	fi, err := os.Lstat(obj)
	return err == nil && fi.Mode().IsRegular() && fi.Size() == size
}

// Smudge is the git smudge filter: given a stub on in, it writes the cached
// content to w, or the stub itself when the object is not cached yet. A size
// mismatch between stub and object is warned about but not fatal.
//
// Input that is not a stub is passed through unchanged. This deliberately
// departs from the Python filter_smudge, which wrote nothing and so emptied
// the checked-out file. A stub whose digest is not a safe object name is
// passed through with a warning instead of failing, because the filter is
// required and a failure would abort the whole checkout.
func (r *Repo) Smudge(in io.Reader, w io.Writer) error {
	// Read one byte past a stub so longer input that starts with a valid stub
	// counts as content, matching how Clean decides what is a stub.
	head, err := readUpTo(in, fatstub.Len+1)
	if err != nil {
		return err
	}
	if !fatstub.IsStub(head) {
		return passThrough(w, head, in)
	}
	sha, size, err := fatstub.Decode(head)
	if err != nil {
		r.logf(false, "filter-smudge: %v, leaving the stub in place", err)
		return passThrough(w, head, in)
	}
	f, err := os.Open(filepath.Join(r.ObjDir, sha))
	if errors.Is(err, fs.ErrNotExist) {
		r.logf(true, "filter-smudge: fat object missing, run: git-fat pull")
		_, err = w.Write(head)
		return err
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }() // read-only; nothing to do about a close error
	n, err := io.Copy(w, f)
	if err != nil {
		return err
	}
	if n != size {
		r.logf(false, "filter-smudge: invalid file size of %s, expected: %d, got: %d", f.Name(), size, n)
	}
	return nil
}

// passThrough writes head and then the rest of in to w unchanged.
func passThrough(w io.Writer, head []byte, in io.Reader) error {
	if _, err := w.Write(head); err != nil {
		return err
	}
	_, err := io.Copy(w, in)
	return err
}
