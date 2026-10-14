package fatrepo_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hurricanehrndz/git-fat/v2/pkg/fatrepo"
	"github.com/hurricanehrndz/git-fat/v2/pkg/fatstub"
)

// TestMain builds git-fat onto PATH, so the repos under test run the real
// filters, and isolates git from the user's config.
func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	bin, err := os.MkdirTemp("", "git-fat-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(bin) }()
	name := "git-fat"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if out, berr := exec.Command("go", "build", "-o", filepath.Join(bin, name), "../../cmd/git-fat").CombinedOutput(); berr != nil {
		fmt.Fprintf(os.Stderr, "go build: %v\n%s", berr, out)
		return 1
	}
	globalCfg := filepath.Join(bin, "gitconfig")
	if err = os.WriteFile(globalCfg, nil, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for k, v := range map[string]string{
		"PATH":                bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL":   globalCfg,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_FAT_VERBOSE":     "",
	} {
		_ = os.Setenv(k, v)
	}
	return m.Run()
}

// memStore is an in-memory Store. corrupt makes downloads serve altered
// bytes; lists counts List calls.
type memStore struct {
	objs    map[string][]byte
	mod     time.Time
	uploads []string
	corrupt bool
	lists   int
}

func newMemStore() *memStore {
	return &memStore{objs: map[string][]byte{}, mod: time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (m *memStore) List(context.Context) (map[string]bool, error) {
	m.lists++
	keys := map[string]bool{}
	for k := range m.objs {
		keys[k] = true
	}
	return keys, nil
}

func (m *memStore) Upload(_ context.Context, localPath, key string) error {
	b, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	m.objs[key] = b
	m.uploads = append(m.uploads, key)
	return nil
}

func (m *memStore) Download(_ context.Context, key string, w io.Writer) (time.Time, error) {
	b, ok := m.objs[key]
	if !ok {
		return time.Time{}, errors.New("no such key " + key)
	}
	if m.corrupt {
		b = append([]byte("X"), b[1:]...)
	}
	_, err := w.Write(b)
	return m.mod, err
}

// run runs a command in dir and returns its stdout, failing the test on error.
func run(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, stderr.String())
	}
	return string(out)
}

// put writes content to dir/rel, creating parent directories.
func put(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// stubOf returns the stub for content.
func stubOf(content string) string {
	return string(fatstub.Encode(sha1hex([]byte(content)), int64(len(content))))
}

// openRepo opens dir as a Repo, which configures the fat filters.
func openRepo(t *testing.T, dir string) (*fatrepo.Repo, *bytes.Buffer) {
	t.Helper()
	stderr := &bytes.Buffer{}
	r, err := fatrepo.Open(context.Background(), dir, stderr)
	if err != nil {
		t.Fatal(err)
	}
	return r, stderr
}

// initRepo creates a repo on branch master. With filter set, *.fat files go
// through the fat filter; otherwise their bytes are committed as they are.
func initRepo(t *testing.T, filter bool) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	run(t, "", "git", "init", "-q", "-b", "master", dir)
	run(t, dir, "git", "config", "user.name", "git-fat test")
	run(t, dir, "git", "config", "user.email", "git-fat@example.com")
	if filter {
		openRepo(t, dir)
		put(t, dir, ".gitattributes", "*.fat filter=fat -crlf\n")
	}
	return dir
}

func commit(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		put(t, dir, rel, files[rel])
	}
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-q", "-m", "commit")
}

func paths(objs []fatrepo.Object) []string {
	var ps []string
	for _, o := range objs {
		ps = append(ps, o.Path)
	}
	return ps
}

// fatClone commits files through the fat filter, pushes their objects to a
// new store, and clones the repo without them, so the clone's fat files are
// checked out as stubs. It returns the clone's Repo.
func fatClone(t *testing.T, files map[string]string) (string, *fatrepo.Repo, *bytes.Buffer, *memStore) {
	t.Helper()
	src := initRepo(t, true)
	commit(t, src, files)
	store := newMemStore()
	r, _ := openRepo(t, src)
	if err := r.Push(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(t.TempDir(), "clone")
	run(t, "", "git", "clone", "-q", src, clone)
	cr, stderr := openRepo(t, clone)
	return clone, cr, stderr, store
}

// TestIndexObjectsExactStubs uses the filters' rule: a blob that starts
// with a stub but is longer is content, which Python's is_fatblob got wrong.
func TestIndexObjectsExactStubs(t *testing.T) {
	dir := initRepo(t, false)
	commit(t, dir, map[string]string{
		"a.fat":     stubOf("fat content a\n"),
		"long.fat":  stubOf("fat content b\n") + "x",
		"plain.txt": "plain\n",
	})
	r, _ := openRepo(t, dir)
	objs, err := r.IndexObjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].Path != "a.fat" || objs[0].SHA != sha1hex([]byte("fat content a\n")) || objs[0].Size != 14 {
		t.Fatalf("IndexObjects = %+v, want only a.fat", objs)
	}
}

// TestNewObjects counts modified fat files as new, and with cached compares
// the index rather than HEAD, as the fscheck-new pre-commit hook needs.
func TestNewObjects(t *testing.T) {
	dir := initRepo(t, false)
	commit(t, dir, map[string]string{"a.fat": stubOf("a1"), "keep.fat": stubOf("k")})
	run(t, dir, "git", "checkout", "-q", "-b", "topic")
	commit(t, dir, map[string]string{"a.fat": stubOf("a2"), "b.fat": stubOf("b"), "plain.txt": "x"})
	put(t, dir, "c.fat", stubOf("c"))
	run(t, dir, "git", "add", "c.fat")
	r, _ := openRepo(t, dir)
	ctx := context.Background()
	for _, tc := range []struct {
		cached bool
		want   []string
	}{
		{false, []string{"a.fat", "b.fat"}},
		{true, []string{"a.fat", "b.fat", "c.fat"}},
	} {
		objs, err := r.NewObjects(ctx, "master", tc.cached)
		if err != nil {
			t.Fatal(err)
		}
		if got := paths(objs); !slices.Equal(got, tc.want) {
			t.Errorf("NewObjects(cached=%v) = %v, want %v", tc.cached, got, tc.want)
		}
		if objs[0].SHA != sha1hex([]byte("a2")) {
			t.Errorf("a.fat SHA = %s, want the new side's", objs[0].SHA)
		}
	}
	if _, err := r.NewObjects(ctx, "no-such-ref", false); err == nil || !strings.Contains(err.Error(), "no-such-ref") {
		t.Fatalf("unknown ref error = %v, want it named", err)
	}
}

func TestPushUploadsOnlyMissing(t *testing.T) {
	dir := initRepo(t, true)
	// ghost.fat is a stub of content never cleaned here: clean passes stubs
	// through, so the object exists nowhere.
	commit(t, dir, map[string]string{"a.fat": "fat a\n", "b.fat": "fat b\n", "ghost.fat": stubOf("ghost")})
	r, stderr := openRepo(t, dir)
	store := newMemStore()
	store.objs[sha1hex([]byte("fat a\n"))] = []byte("fat a\n")
	ctx := context.Background()
	if err := r.Push(ctx, store); err != nil {
		t.Fatal(err)
	}
	bSHA := sha1hex([]byte("fat b\n"))
	if !slices.Equal(store.uploads, []string{bSHA}) || string(store.objs[bSHA]) != "fat b\n" {
		t.Fatalf("uploads = %v, want only b.fat's object", store.uploads)
	}
	if !strings.Contains(stderr.String(), "git-fat push: uploading b.fat\n") ||
		!strings.Contains(stderr.String(), "ghost.fat is neither cached locally nor on the remote store") {
		t.Fatalf("stderr = %q", stderr)
	}
	stderr.Reset()
	if err := r.Push(ctx, store); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "git-fat push: nothing to push\n") || len(store.uploads) != 1 {
		t.Fatalf("second push: uploads %v, stderr %q", store.uploads, stderr)
	}
}

func TestPullRestoresStubs(t *testing.T) {
	clone, r, stderr, store := fatClone(t, map[string]string{"a.fat": "fat a\n", "b.fat": "fat b\n"})
	if get(t, clone, "a.fat") != stubOf("fat a\n") {
		t.Fatal("clone did not check out a stub")
	}
	if err := os.Chmod(filepath.Join(clone, "a.fat"), 0o600); err != nil {
		t.Fatal(err)
	}
	put(t, clone, "b.fat", "local edit\n")
	ctx := context.Background()
	objs, err := r.IndexObjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Pull(ctx, "pull", store, objs); err != nil {
		t.Fatalf("Pull: %v\n%s", err, stderr)
	}
	if got := get(t, clone, "a.fat"); got != "fat a\n" {
		t.Fatalf("a.fat = %q, want restored content", got)
	}
	if got := get(t, clone, "b.fat"); got != "local edit\n" {
		t.Fatalf("b.fat = %q, want the local edit kept", got)
	}
	fi, err := os.Stat(filepath.Join(clone, "a.fat"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("a.fat mode = %v, want the working file's 0600", fi.Mode().Perm())
	}
	if !fi.ModTime().Equal(store.mod) {
		t.Errorf("a.fat mtime = %v, want the store's %v", fi.ModTime(), store.mod)
	}
	obj, err := os.Stat(filepath.Join(r.ObjDir, sha1hex([]byte("fat a\n"))))
	if err != nil || !obj.ModTime().Equal(store.mod) {
		t.Errorf("cached object: %v, mtime %v; want the store's mtime", err, obj.ModTime())
	}
	if !strings.Contains(stderr.String(), "git-fat pull: restore a.fat from "+sha1hex([]byte("fat a\n"))) {
		t.Errorf("stderr = %q", stderr)
	}
	// diff-files does not refresh the index, so it shows whether pull did.
	if got := run(t, clone, "git", "diff-files", "--name-only"); got != "b.fat\n" {
		t.Errorf("stat-dirty files = %q, want only the edited b.fat", got)
	}
	if got := run(t, clone, "git", "status", "--porcelain"); got != " M b.fat\n" {
		t.Errorf("status = %q, want only b.fat modified", got)
	}
}

// TestPullRestoresCachedObject covers an object copied in by hand: Python
// left the stub in place because nothing needed downloading.
func TestPullRestoresCachedObject(t *testing.T) {
	clone, r, _, _ := fatClone(t, map[string]string{"a.fat": "fat a\n"})
	put(t, r.ObjDir, sha1hex([]byte("fat a\n")), "fat a\n")
	store := newMemStore()
	ctx := context.Background()
	objs, err := r.IndexObjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Pull(ctx, "pull", store, objs); err != nil {
		t.Fatal(err)
	}
	if got := get(t, clone, "a.fat"); got != "fat a\n" || store.lists != 0 {
		t.Fatalf("a.fat = %q after %d List calls, want content and no listing", got, store.lists)
	}
}

// TestPullRejectsBadDownload keeps a corrupt or missing object out of the
// cache and the working tree.
func TestPullRejectsBadDownload(t *testing.T) {
	for _, tc := range []struct{ name, msg string }{
		{"corrupt", "git-fat pull: a.fat: downloaded content has digest"},
		{"missing", "git-fat pull: a.fat not found on remote store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clone, r, stderr, store := fatClone(t, map[string]string{"a.fat": "fat a\n", "b.fat": "fat b\n"})
			aSHA := sha1hex([]byte("fat a\n"))
			if tc.name == "corrupt" {
				store.corrupt = true
			} else {
				delete(store.objs, aSHA)
			}
			ctx := context.Background()
			objs, err := r.IndexObjects(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = r.Pull(ctx, "pull", store, objs); err == nil {
				t.Fatal("Pull succeeded")
			}
			if !strings.Contains(stderr.String(), tc.msg) {
				t.Fatalf("stderr = %q, want %q", stderr, tc.msg)
			}
			if got := get(t, clone, "a.fat"); got != stubOf("fat a\n") {
				t.Fatalf("a.fat = %q, want the stub", got)
			}
			var want []string
			if tc.name == "missing" {
				// b.fat still pulls: one failure does not stop the rest.
				want = []string{sha1hex([]byte("fat b\n"))}
			}
			assertOnlyObjects(t, r.ObjDir, want...)
		})
	}
}

// TestPullFromSubdirectory resolves file arguments against the current
// directory, as a user in a subdirectory types them.
func TestPullFromSubdirectory(t *testing.T) {
	clone, _, _, store := fatClone(t, map[string]string{"sub/c.fat": "fat c\n", "sub/plain.txt": "plain\n"})
	r, stderr := openRepo(t, filepath.Join(clone, "sub"))
	ctx := context.Background()
	ps, err := r.ResolvePaths(ctx, "pull", []string{"c.fat", "plain.txt", "missing.fat"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ps, []string{"sub/c.fat", "sub/plain.txt"}) {
		t.Fatalf("ResolvePaths = %q", ps)
	}
	objs, err := r.HeadObjects(ctx, "pull", ps)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Pull(ctx, "pull", store, objs); err != nil {
		t.Fatal(err)
	}
	if got := get(t, clone, "sub/c.fat"); got != "fat c\n" {
		t.Fatalf("sub/c.fat = %q", got)
	}
	for _, msg := range []string{
		"git-fat pull: missing.fat not found, is given path valid?\n",
		"git-fat pull: sub/plain.txt is not a fat object\n",
	} {
		if !strings.Contains(stderr.String(), msg) {
			t.Errorf("stderr = %q, want %q", stderr, msg)
		}
	}
	if _, err = r.ResolvePaths(ctx, "pull", []string{"missing.fat"}); err == nil {
		t.Fatal("ResolvePaths succeeded with no valid path")
	}
}

func TestCheckNamesMissing(t *testing.T) {
	_, r, stderr, store := fatClone(t, map[string]string{"a.fat": "fat a\n", "b.fat": "fat b\n"})
	delete(store.objs, sha1hex([]byte("fat b\n")))
	ctx := context.Background()
	objs, err := r.IndexObjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Check(ctx, "fscheck", store, objs); err == nil {
		t.Fatal("Check succeeded with b.fat missing")
	}
	if got := stderr.String(); got != "git-fat fscheck: b.fat not found on remote store\n" {
		t.Fatalf("stderr = %q", got)
	}
	if err = r.Check(ctx, "fscheck", store, objs[:1]); err != nil {
		t.Fatalf("Check of a.fat: %v", err)
	}
}

// TestPublishUsesPaths keys smudge-store objects by repo path, downloading
// an object that is not cached first.
func TestPublishUsesPaths(t *testing.T) {
	dir := initRepo(t, true)
	commit(t, dir, map[string]string{"a.fat": "fat a\n"})
	run(t, dir, "git", "checkout", "-q", "-b", "topic")
	commit(t, dir, map[string]string{"test dir/f.fat": "fat f\n"})
	r, stderr := openRepo(t, dir)
	ctx := context.Background()
	store, smudge := newMemStore(), newMemStore()
	if err := r.Push(ctx, store); err != nil {
		t.Fatal(err)
	}
	fSHA := sha1hex([]byte("fat f\n"))
	if err := os.Remove(filepath.Join(r.ObjDir, fSHA)); err != nil {
		t.Fatal(err)
	}
	objs, err := r.NewObjects(ctx, "master", false)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Publish(ctx, store, smudge, objs); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(smudge.uploads, []string{"test dir/f.fat"}) || string(smudge.objs["test dir/f.fat"]) != "fat f\n" {
		t.Fatalf("smudge store uploads = %v", smudge.uploads)
	}
	if !strings.Contains(stderr.String(), "git-fat fspublish-new: publishing 'test dir/f.fat' to smudgestore\n") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// TestPullKeepsStagedChange restores HEAD's object over a working stub while
// the index holds a different staged version, which must stay staged.
func TestPullKeepsStagedChange(t *testing.T) {
	clone, r, stderr, store := fatClone(t, map[string]string{"a.fat": "fat a\n"})
	put(t, clone, "a.fat", "staged\n")
	run(t, clone, "git", "add", "a.fat")
	staged := run(t, clone, "git", "rev-parse", ":a.fat")
	put(t, clone, "a.fat", stubOf("fat a\n"))
	ctx := context.Background()
	objs, err := r.HeadObjects(ctx, "pull", []string{"a.fat"})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Pull(ctx, "pull", store, objs); err != nil {
		t.Fatal(err)
	}
	if got := get(t, clone, "a.fat"); got != "fat a\n" {
		t.Fatalf("a.fat = %q, want HEAD's content", got)
	}
	if got := run(t, clone, "git", "rev-parse", ":a.fat"); got != staged {
		t.Fatalf("index a.fat = %s, want the staged %s", got, staged)
	}
	if !strings.Contains(stderr.String(), "differs from its staged version") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// TestPullKeepsStagedMode keeps a staged mode change: pull must not stage
// the working file's mode along with refreshing it.
func TestPullKeepsStagedMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no executable bit on Windows")
	}
	clone, r, stderr, store := fatClone(t, map[string]string{"a.fat": "fat a\n"})
	run(t, clone, "git", "config", "core.filemode", "true")
	run(t, clone, "git", "update-index", "--chmod=+x", "a.fat")
	ctx := context.Background()
	objs, err := r.IndexObjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Pull(ctx, "pull", store, objs); err != nil {
		t.Fatalf("Pull: %v\n%s", err, stderr)
	}
	if got := get(t, clone, "a.fat"); got != "fat a\n" {
		t.Fatalf("a.fat = %q, want restored content", got)
	}
	if got := run(t, clone, "git", "ls-files", "-s", "a.fat"); !strings.HasPrefix(got, "100755 "+headBlob(t, clone, "a.fat")+" 0\t") {
		t.Fatalf("index a.fat = %q, want the staged 100755 stub", got)
	}
}

// TestPullWithoutFilterKeepsStub refreshes with the fat filter gone from
// .gitattributes: the restored content must not be staged in the stub's place.
func TestPullWithoutFilterKeepsStub(t *testing.T) {
	clone, r, stderr, store := fatClone(t, map[string]string{"a.fat": "fat a\n"})
	put(t, clone, ".gitattributes", "")
	ctx := context.Background()
	objs, err := r.IndexObjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Pull(ctx, "pull", store, objs); err != nil {
		t.Fatalf("Pull: %v\n%s", err, stderr)
	}
	if got := strings.TrimSpace(run(t, clone, "git", "rev-parse", ":a.fat")); got != headBlob(t, clone, "a.fat") {
		t.Fatalf("index a.fat = %s, want the stub %s", got, headBlob(t, clone, "a.fat"))
	}
}

// TestPullEditBeforeRefresh edits a restored file before the index is
// refreshed: the edit must stay unstaged.
func TestPullEditBeforeRefresh(t *testing.T) {
	clone, r, stderr, store := fatClone(t, map[string]string{"a.fat": "fat a\n"})
	fatrepo.SetTestHook(t, func(event, _ string) {
		if event == "restored" {
			put(t, clone, "a.fat", "edited\n")
		}
	})
	ctx := context.Background()
	objs, err := r.IndexObjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Pull(ctx, "pull", store, objs); err != nil {
		t.Fatalf("Pull: %v\n%s", err, stderr)
	}
	if got := strings.TrimSpace(run(t, clone, "git", "rev-parse", ":a.fat")); got != headBlob(t, clone, "a.fat") {
		t.Fatalf("index a.fat = %s, want the stub %s", got, headBlob(t, clone, "a.fat"))
	}
	if got := run(t, clone, "git", "status", "--porcelain"); got != " M a.fat\n" {
		t.Fatalf("status = %q, want a.fat modified", got)
	}
}

// TestPullEditDuringCopy edits the stub while its object is being copied:
// the edit must survive and the file count as failed.
func TestPullEditDuringCopy(t *testing.T) {
	clone, r, stderr, store := fatClone(t, map[string]string{"a.fat": "fat a\n", "b.fat": "fat b\n"})
	fatrepo.SetTestHook(t, func(event, path string) {
		if event == "copied" && filepath.Base(path) == "a.fat" {
			put(t, clone, "a.fat", "edited\n")
		}
	})
	ctx := context.Background()
	objs, err := r.IndexObjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Pull(ctx, "pull", store, objs); err == nil || !strings.Contains(err.Error(), "1 fat object(s) failed") {
		t.Fatalf("Pull error = %v, want one failure", err)
	}
	if got := get(t, clone, "a.fat"); got != "edited\n" {
		t.Fatalf("a.fat = %q, want the edit kept", got)
	}
	if got := get(t, clone, "b.fat"); got != "fat b\n" {
		t.Fatalf("b.fat = %q, want restored content", got)
	}
	if !strings.Contains(stderr.String(), "git-fat pull: restoring a.fat: file changed during restore") {
		t.Fatalf("stderr = %q", stderr)
	}
	if left, _ := filepath.Glob(filepath.Join(clone, ".git-fat-tmp-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	if got := run(t, clone, "git", "status", "--porcelain"); got != " M a.fat\n" {
		t.Fatalf("status = %q, want only a.fat modified", got)
	}
}

// TestPullReplacesReadOnly restores over a read-only working stub and
// replaces a wrong-sized read-only cached object, which on Windows a plain
// rename refuses to do.
func TestPullReplacesReadOnly(t *testing.T) {
	clone, r, stderr, store := fatClone(t, map[string]string{"a.fat": "fat a\n"})
	obj := filepath.Join(r.ObjDir, sha1hex([]byte("fat a\n")))
	put(t, r.ObjDir, sha1hex([]byte("fat a\n")), "short")
	for _, p := range []string{obj, filepath.Join(clone, "a.fat")} {
		if err := os.Chmod(p, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	objs, err := r.IndexObjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Pull(ctx, "pull", store, objs); err != nil {
		t.Fatalf("Pull: %v\n%s", err, stderr)
	}
	if got := get(t, clone, "a.fat"); got != "fat a\n" {
		t.Fatalf("a.fat = %q, want restored content", got)
	}
	if got := get(t, r.ObjDir, sha1hex([]byte("fat a\n"))); got != "fat a\n" {
		t.Fatalf("cached object = %q, want the download", got)
	}
}

// headBlob returns the blob HEAD records for rel.
func headBlob(t *testing.T, dir, rel string) string {
	t.Helper()
	return strings.TrimSpace(run(t, dir, "git", "rev-parse", "HEAD:"+rel))
}

// TestPullDuringMerge refreshes an index with an unmerged entry, which must
// not fail the pull.
func TestPullDuringMerge(t *testing.T) {
	clone, r, stderr, store := fatClone(t, map[string]string{"a.fat": "fat a\n", "c.txt": "base\n"})
	run(t, clone, "git", "config", "user.name", "git-fat test")
	run(t, clone, "git", "config", "user.email", "git-fat@example.com")
	run(t, clone, "git", "checkout", "-q", "-b", "topic")
	commit(t, clone, map[string]string{"c.txt": "topic\n"})
	run(t, clone, "git", "checkout", "-q", "master")
	commit(t, clone, map[string]string{"c.txt": "master\n"})
	if err := exec.Command("git", "-C", clone, "merge", "-q", "topic").Run(); err == nil {
		t.Fatal("merge did not conflict")
	}
	ctx := context.Background()
	objs, err := r.IndexObjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Pull(ctx, "pull", store, objs); err != nil {
		t.Fatalf("Pull: %v\n%s", err, stderr)
	}
	if got := run(t, clone, "git", "status", "--porcelain"); got != "UU c.txt\n" {
		t.Fatalf("status = %q, want only the conflict", got)
	}
}
