package fatrepo

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hurricanehrndz/git-fat/pkg/fatstub"
)

// Store is the remote store the commands move objects to and from.
// *fatstore.Store implements it; tests substitute an in-memory one.
type Store interface {
	// List returns every key in the store.
	List(ctx context.Context) (map[string]bool, error)
	// Upload stores the file at localPath under key.
	Upload(ctx context.Context, localPath, key string) error
	// Download writes the object under key to w and returns its
	// modification time.
	Download(ctx context.Context, key string, w io.Writer) (time.Time, error)
}

// remote lists a store at most once, and only when first asked.
type remote struct {
	s    Store
	keys map[string]bool
}

func (rm *remote) has(ctx context.Context, key string) (bool, error) {
	if rm.keys == nil {
		keys, err := rm.s.List(ctx)
		if err != nil {
			return false, err
		}
		rm.keys = keys
	}
	return rm.keys[key], nil
}

// errNotOnRemote reports an object that is neither cached nor in the store.
var errNotOnRemote = errors.New("not found on remote store")

// logObjErr reports err for o, in Python's wording for errNotOnRemote.
func (r *Repo) logObjErr(cmd string, o Object, err error) {
	if errors.Is(err, errNotOnRemote) {
		r.logf(false, "%s: %s not found on remote store", cmd, o.Path)
		return
	}
	r.logf(false, "%s: %s: %v", cmd, o.Path, err)
}

// objPath returns where o is cached.
func (r *Repo) objPath(o Object) string {
	return filepath.Join(r.ObjDir, o.SHA)
}

// failed reports n failed objects for cmd as one error, or nil for none.
func failed(cmd string, n int, what string) error {
	if n == 0 {
		return nil
	}
	return fmt.Errorf("%s: %d fat object(s) %s", cmd, n, what)
}

// Push uploads the index's fat objects that are cached here but missing from
// s, keyed by their digest. An object neither cached nor in s is warned
// about: nobody can smudge it. An upload failure does not stop the others.
func (r *Repo) Push(ctx context.Context, s Store) error {
	objs, err := r.IndexObjects(ctx)
	if err != nil {
		return err
	}
	keys, err := s.List(ctx)
	if err != nil {
		return err
	}
	var todo []Object
	queued := map[string]bool{}
	for _, o := range objs {
		switch {
		case keys[o.SHA] || queued[o.SHA]:
		case !cached(r.objPath(o), o.Size):
			r.logf(false, "push: %s is neither cached locally nor on the remote store", o.Path)
		default:
			queued[o.SHA] = true
			todo = append(todo, o)
		}
	}
	if len(todo) == 0 {
		r.logf(false, "push: nothing to push")
		return nil
	}
	nfail := 0
	for _, o := range todo {
		if err = ctx.Err(); err != nil {
			return err
		}
		r.logf(false, "push: uploading %s", o.Path)
		if err = s.Upload(ctx, r.objPath(o), o.SHA); err != nil {
			r.logf(false, "push: %s: %v", o.Path, err)
			nfail++
		}
	}
	return failed("push", nfail, "failed to upload")
}

// Pull makes sure each of objs is cached, downloading it from s when not,
// and restores each working file that still holds its stub. cmd names the
// command in messages. A failed object does not stop the others.
//
// Python skipped restoring an object already cached, which left the stub in
// place after a manual copy; here any working stub is restored.
func (r *Repo) Pull(ctx context.Context, cmd string, s Store, objs []Object) error {
	top, err := r.Toplevel(ctx)
	if err != nil {
		return err
	}
	rm := &remote{s: s}
	var restored []Object
	downloads, nfail := 0, 0
	for _, o := range objs {
		if ctx.Err() != nil {
			break
		}
		got, ferr := r.ensureCached(ctx, cmd, rm, o)
		if ferr != nil {
			r.logObjErr(cmd, o, ferr)
			nfail++
			continue
		}
		if got {
			downloads++
		}
		ok, rerr := r.restore(cmd, top, o)
		if rerr != nil {
			r.logf(false, "%s: restoring %s: %v", cmd, o.Path, rerr)
			nfail++
		}
		if ok {
			restored = append(restored, o)
		}
	}
	if testHook != nil {
		testHook("restored", top)
	}
	// Files already restored are refreshed even after an interrupt, or they
	// would show as modified.
	if err = r.refresh(context.WithoutCancel(ctx), cmd, top, restored); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if downloads == 0 && len(restored) == 0 && nfail == 0 {
		r.logf(false, "%s: nothing to pull", cmd)
	}
	return failed(cmd, nfail, "failed to pull")
}

// ensureCached downloads o from rm unless it is cached already, and reports
// whether it downloaded. An object rm lacks is an error.
func (r *Repo) ensureCached(ctx context.Context, cmd string, rm *remote, o Object) (bool, error) {
	if cached(r.objPath(o), o.Size) {
		return false, nil
	}
	ok, err := rm.has(ctx, o.SHA)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, errNotOnRemote
	}
	r.logf(true, "%s: downloading %s", cmd, o.SHA)
	return true, r.download(ctx, rm.s, o)
}

// download fetches o from s into ObjDir via a temp file, which is published
// only if its digest and size match the stub. The object gets the store's
// modification time and the same read-only mode Clean gives objects.
func (r *Repo) download(ctx context.Context, s Store, o Object) error {
	tmp, err := os.CreateTemp(r.ObjDir, "tmp-")
	if err != nil {
		return err
	}
	defer func() {
		// After a successful rename this fails harmlessly with ErrNotExist.
		_ = os.Remove(tmp.Name())
	}()
	h := sha1.New()
	var n counter
	mod, err := s.Download(ctx, o.SHA, io.MultiWriter(tmp, h, &n))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if sha := hex.EncodeToString(h.Sum(nil)); sha != o.SHA || int64(n) != o.Size {
		return fmt.Errorf("downloaded content has digest %s and size %d, want %s and %d", sha, n, o.SHA, o.Size)
	}
	// Set the time before making the file read-only, which on Windows
	// would forbid it.
	if !mod.IsZero() {
		if err = os.Chtimes(tmp.Name(), time.Time{}, mod); err != nil {
			return err
		}
	}
	if err = os.Chmod(tmp.Name(), 0o444&^umask()); err != nil {
		return err
	}
	return replaceFile(tmp.Name(), r.objPath(o))
}

// counter counts the bytes written to it.
type counter int64

func (c *counter) Write(p []byte) (int, error) {
	*c += counter(len(p))
	return len(p), nil
}

// errChanged reports a working file that stopped being its stub while it
// was being restored.
var errChanged = errors.New("file changed during restore, left as is")

// testHook, when set by a test, is called with an event name and a path at
// points where the test needs to interfere.
var testHook func(event, path string)

// restore replaces o's working file with the cached content, but only while
// the file holds o's stub, so local edits are never overwritten. It reports
// whether it restored. The file keeps its own permission bits; Python's
// copy2 gave it the object's read-only mode. It takes the object's mtime.
func (r *Repo) restore(cmd, top string, o Object) (bool, error) {
	path := filepath.Join(top, filepath.FromSlash(o.Path))
	fi, ok, err := holdsStub(path, o)
	if err != nil {
		return false, err
	}
	if !ok {
		r.logf(true, "%s: %s is not its stub, leaving it alone", cmd, o.Path)
		return false, nil
	}
	r.logf(false, "%s: restore %s from %s", cmd, o.Path, o.SHA)
	err = r.replace(path, fi.Mode().Perm(), o)
	return err == nil, err
}

// holdsStub reports whether path is a regular file holding exactly o's stub,
// and returns its info.
func holdsStub(path string, o Object) (os.FileInfo, bool, error) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != fatstub.Len {
		return nil, false, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	sha, _, derr := fatstub.Decode(b)
	return fi, derr == nil && fatstub.IsStub(b) && sha == o.SHA, nil
}

// replace atomically overwrites path, which holds o's stub, with a copy of
// o's cached object, giving it perm and the object's mtime.
func (r *Repo) replace(path string, perm os.FileMode, o Object) error {
	src, err := os.Open(r.objPath(o))
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }() // read-only; nothing to do about a close error
	srcInfo, err := src.Stat()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".git-fat-tmp-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = io.Copy(tmp, src)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err = os.Chmod(tmp.Name(), perm); err != nil {
		return err
	}
	if err = os.Chtimes(tmp.Name(), time.Time{}, srcInfo.ModTime()); err != nil {
		return err
	}
	if testHook != nil {
		testHook("copied", path)
	}
	// The copy of a large object takes a while, so check again that the file
	// still holds the stub right before replacing it. A write landing between
	// this check and the rename is still lost: no portable rename replaces a
	// file only if it is unchanged. That window is two system calls wide.
	if _, ok, err := holdsStub(path, o); err != nil {
		return err
	} else if !ok {
		return errChanged
	}
	return replaceFile(tmp.Name(), path)
}

// refresh updates the index's stat data for the restored objects, which
// still describes their stubs, so git status shows them clean. It never
// stages working-tree content.
//
// A plain update-index --refresh leaves the files dirty: git takes the size
// change as a modification without running the clean filter. So the entries
// are first rewritten unchanged through --index-info, which zeroes their stat
// data; with no recorded size, --refresh compares each file through the
// clean filter and, when that reproduces the stub, only records the new stat
// data. A file the filter does not turn back into the stub (attributes
// removed, edited since the restore) just stays modified.
//
// Only entries that still hold the mode and blob restored are rewritten; any
// other is a staged change and is left alone. The index is read whole, so no
// paths go on the command line.
func (r *Repo) refresh(ctx context.Context, cmd, top string, restored []Object) error {
	if len(restored) == 0 {
		return nil
	}
	out, err := gitBytes(ctx, top, nil, "ls-files", "-s", "-z")
	if err != nil {
		return err
	}
	indexed := map[string]string{}
	for _, rec := range splitZ(out) {
		// <mode> <blob> <stage>\t<path>
		meta, path, _ := strings.Cut(rec, "\t")
		if f := strings.Fields(meta); len(f) == 3 && f[2] == "0" {
			indexed[path] = f[0] + " " + f[1]
		}
	}
	var in bytes.Buffer
	for _, o := range restored {
		if e := o.mode + " " + o.blob; indexed[o.Path] == e {
			in.WriteString(e + "\t" + o.Path + "\x00")
		} else {
			r.logf(false, "%s: restored %s differs from its staged version, which is left as is", cmd, o.Path)
		}
	}
	if in.Len() == 0 {
		return nil
	}
	if _, err = gitBytes(ctx, top, in.Bytes(), "update-index", "-z", "--index-info"); err != nil {
		return err
	}
	// Other modified (-q) or unmerged (--unmerged) entries are not an error
	// here; the latter would otherwise fail a pull during a merge.
	_, err = gitBytes(ctx, top, nil, "update-index", "-q", "--unmerged", "--refresh")
	return err
}

// Check reports each of objs missing from s, naming cmd, and fails if any
// is. With no objects it does not contact the store.
func (r *Repo) Check(ctx context.Context, cmd string, s Store, objs []Object) error {
	if len(objs) == 0 {
		return nil
	}
	keys, err := s.List(ctx)
	if err != nil {
		return err
	}
	missing := 0
	for _, o := range objs {
		if !keys[o.SHA] {
			r.logf(false, "%s: %s not found on remote store", cmd, o.Path)
			missing++
		}
	}
	return failed(cmd, missing, "not found on remote store")
}

// Publish uploads each of objs to the smudge store keyed by its
// repo-relative path, downloading it from the fat store s first when it is
// not cached. Unlike Python it does not restore the working file on the
// way. A failed object does not stop the others.
func (r *Repo) Publish(ctx context.Context, s, smudge Store, objs []Object) error {
	const cmd = "fspublish-new"
	rm := &remote{s: s}
	nfail := 0
	for _, o := range objs {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := r.ensureCached(ctx, cmd, rm, o)
		if err == nil {
			r.logf(false, "%s: publishing '%s' to smudgestore", cmd, o.Path)
			err = smudge.Upload(ctx, r.objPath(o), o.Path)
		}
		if err != nil {
			r.logObjErr(cmd, o, err)
			nfail++
		}
	}
	return failed(cmd, nfail, "failed to publish")
}
