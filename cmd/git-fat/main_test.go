package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hurricanehrndz/git-fat/v2/pkg/fatstub"
)

func TestRunVersion(t *testing.T) {
	for _, arg := range []string{"version", "-v", "--version"} {
		var out bytes.Buffer
		if err := run(context.Background(), []string{arg}, nil, &out, &bytes.Buffer{}); err != nil {
			t.Fatalf("%s: %v", arg, err)
		}
		if out.String() != version+"\n" {
			t.Fatalf("%s printed %q", arg, out.String())
		}
	}
}

func TestRunUsageErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}, {"filter-clean", "extra"}} {
		err := run(context.Background(), args, nil, &bytes.Buffer{}, &bytes.Buffer{})
		if err == nil {
			t.Errorf("run(%q) succeeded, want usage error", args)
		}
	}
}

// TestRunStoreUsageErrors are rejected before git-fat looks for a repo, so
// they never touch the repo the test runs in.
func TestRunStoreUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"push", "extra"}, {"pull"}, {"pull", "-x"}, {"pull-new", "a", "b"}, {"fscheck-new", "a", "b"}} {
		var stderr bytes.Buffer
		if err := run(context.Background(), args, nil, &bytes.Buffer{}, &stderr); err == nil {
			t.Errorf("run(%q) succeeded, want usage error", args)
		}
	}
}

func TestParseStoreArgs(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want storeArgs
	}{
		// --all selects the whole index even with files named.
		{[]string{"pull", "a.fat", "--all"}, storeArgs{all: true}},
		{[]string{"pull", "-a"}, storeArgs{all: true}},
		{[]string{"pull", "--", "-a"}, storeArgs{files: []string{"-a"}}},
		{[]string{"fscheck", "a.fat", "b.fat"}, storeArgs{files: []string{"a.fat", "b.fat"}}},
		{[]string{"fscheck-new"}, storeArgs{ref: "master"}},
		{[]string{"pull-new", "main"}, storeArgs{ref: "main"}},
	} {
		got, err := parseStoreArgs(tc.args[0], tc.args[1:], &bytes.Buffer{})
		if err != nil || got.all != tc.want.all || got.ref != tc.want.ref || strings.Join(got.files, ",") != strings.Join(tc.want.files, ",") {
			t.Errorf("parseStoreArgs(%q) = %+v, %v; want %+v", tc.args, got, err, tc.want)
		}
	}
}

// buildGitFat builds the binary as git-fat on a fresh PATH entry so that
// `git fat ...` resolves to it, and isolates git from the user's config.
func buildGitFat(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	name := "git-fat"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", filepath.Join(bin, name), ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	globalCfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(globalCfg, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_FAT_VERBOSE", "")
}

// sh runs a command in dir and returns its stdout, failing the test on error.
func sh(t *testing.T, dir string, name string, args ...string) string {
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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// newFatRepo creates a git repo with git-fat initialised and a.fat committed.
func newFatRepo(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	sh(t, "", "git", "init", "-q", src)
	sh(t, src, "git", "config", "user.name", "git-fat test")
	sh(t, src, "git", "config", "user.email", "git-fat@example.com")
	sh(t, src, "git", "fat", "init")
	writeFile(t, filepath.Join(src, ".gitattributes"), "*.fat filter=fat -crlf\n")
	writeFile(t, filepath.Join(src, "a.fat"), "fat content a\n")
	sh(t, src, "git", "add", ".gitattributes", "a.fat")
	sh(t, src, "git", "commit", "-q", "-m", "add a.fat")
	return src
}

// TestEndToEnd drives the real binary through git: commit stores the
// Python-compatible stub, a fresh clone checks out the stub until the object
// is present, and then checks out the full content with a clean status.
func TestEndToEnd(t *testing.T) {
	buildGitFat(t)
	src := newFatRepo(t)
	golden := readFile(t, filepath.Join("..", "..", "pkg", "fatstub", "testdata", "a.fat.stub"))
	if got := sh(t, src, "git", "cat-file", "-p", "HEAD:a.fat"); got != golden {
		t.Fatalf("committed blob = %q, want %q", got, golden)
	}
	sha := strings.Fields(golden)[2]

	clone := filepath.Join(t.TempDir(), "clone")
	sh(t, "", "git", "clone", "-q", src, clone)
	sh(t, clone, "git", "fat", "init")
	restore := func() {
		if err := os.Remove(filepath.Join(clone, "a.fat")); err != nil {
			t.Fatal(err)
		}
		sh(t, clone, "git", "checkout", "--", "a.fat")
	}
	restore()
	if got := readFile(t, filepath.Join(clone, "a.fat")); got != golden {
		t.Fatalf("clone a.fat without object = %q, want stub", got)
	}

	obj := readFile(t, filepath.Join(src, ".git", "fat", "objects", sha))
	writeFile(t, filepath.Join(clone, ".git", "fat", "objects", sha), obj)
	restore()
	if got := readFile(t, filepath.Join(clone, "a.fat")); got != "fat content a\n" {
		t.Fatalf("clone a.fat with object = %q, want full content", got)
	}
	if st := sh(t, clone, "git", "status", "--porcelain"); st != "" {
		t.Fatalf("clone status not clean:\n%s", st)
	}
}

// processOnly makes the process filter the only working one in dir. The
// one-shot commands are set to `false` rather than unset: unsetting
// filter.fat.clean would make the next git-fat start auto-init and restore
// them, and with the filter required a fallback to `false` fails loudly.
func processOnly(t *testing.T, dir string) {
	t.Helper()
	if got := sh(t, dir, "git", "config", "--get", "filter.fat.process"); got != "git fat filter-process\n" {
		t.Fatalf("filter.fat.process = %q", got)
	}
	sh(t, dir, "git", "config", "--local", "filter.fat.clean", "false")
	sh(t, dir, "git", "config", "--local", "filter.fat.smudge", "false")
}

// TestFilterProcess drives several files, one larger than a packet, through
// the long-running filter in both directions.
func TestFilterProcess(t *testing.T) {
	buildGitFat(t)
	src := newFatRepo(t)
	processOnly(t, src)
	files := map[string]string{
		"b.fat":   "fat content b\n",
		"c.fat":   "fat content c\n",
		"big.fat": strings.Repeat("0123456789abcdef", 20000), // 320000 bytes
	}
	for name, content := range files {
		writeFile(t, filepath.Join(src, name), content)
	}
	sh(t, src, "git", "add", ".")
	sh(t, src, "git", "commit", "-q", "-m", "add more")
	files["a.fat"] = "fat content a\n"
	for name := range files {
		if blob := sh(t, src, "git", "cat-file", "-p", "HEAD:"+name); !fatstub.IsStub([]byte(blob)) {
			t.Fatalf("committed %s is not a stub: %.80q", name, blob)
		}
	}

	clone := filepath.Join(t.TempDir(), "clone")
	sh(t, "", "git", "clone", "-q", src, clone)
	sh(t, clone, "git", "fat", "init")
	processOnly(t, clone)
	objs := filepath.Join(".git", "fat", "objects")
	entries, err := os.ReadDir(filepath.Join(src, objs))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		writeFile(t, filepath.Join(clone, objs, e.Name()), readFile(t, filepath.Join(src, objs, e.Name())))
	}
	for name := range files {
		if err = os.Remove(filepath.Join(clone, name)); err != nil {
			t.Fatal(err)
		}
	}
	sh(t, clone, "git", "checkout", "--", ".")
	for name, content := range files {
		if got := readFile(t, filepath.Join(clone, name)); got != content {
			t.Fatalf("clone %s = %d bytes, want %d", name, len(got), len(content))
		}
	}
	if st := sh(t, clone, "git", "status", "--porcelain"); st != "" {
		t.Fatalf("clone status not clean:\n%s", st)
	}
}

// TestFilterProcessLargeNonStub checks out a blob committed before the filter
// applied. Git sends it whole before reading the reply, so a filter that
// answers while input is pending deadlocks on the full pipes.
func TestFilterProcessLargeNonStub(t *testing.T) {
	buildGitFat(t)
	src := newFatRepo(t)
	processOnly(t, src)
	attrs := filepath.Join(src, ".gitattributes")
	content := strings.Repeat("0123456789abcdef", 3<<16) // 3 MiB
	writeFile(t, attrs, "")
	writeFile(t, filepath.Join(src, "big.fat"), content)
	sh(t, src, "git", "add", "big.fat")
	sh(t, src, "git", "commit", "-q", "-m", "add big.fat unfiltered")
	writeFile(t, attrs, "*.fat filter=fat -crlf\n")
	if blob := sh(t, src, "git", "cat-file", "-p", "HEAD:big.fat"); blob != content {
		t.Fatalf("committed big.fat is %d bytes, want the %d-byte content", len(blob), len(content))
	}

	if err := os.Remove(filepath.Join(src, "big.fat")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "checkout", "--", "big.fat")
	cmd.Dir = src
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout: %v (ctx: %v)\n%s", err, ctx.Err(), out)
	}
	if got := readFile(t, filepath.Join(src, "big.fat")); got != content {
		t.Fatalf("big.fat = %d bytes, want %d", len(got), len(content))
	}
	tmps, err := filepath.Glob(filepath.Join(src, ".git", "fat", "objects", "tmp-*"))
	if err != nil || len(tmps) != 0 {
		t.Fatalf("temp files left behind: %q, %v", tmps, err)
	}
}

// TestLinkedWorktree checks objects land in the common dir: in a linked
// worktree .git is a file, so Python's <toplevel>/.git/fat path breaks.
func TestLinkedWorktree(t *testing.T) {
	buildGitFat(t)
	src := newFatRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	sh(t, src, "git", "worktree", "add", "-q", wt)
	if got := readFile(t, filepath.Join(wt, "a.fat")); got != "fat content a\n" {
		t.Fatalf("worktree a.fat = %q, want full content", got)
	}
	writeFile(t, filepath.Join(wt, "b.fat"), "fat content b\n")
	sh(t, wt, "git", "add", "b.fat")
	stub := sh(t, wt, "git", "cat-file", "-p", ":b.fat")
	sha := strings.Fields(stub)[2]
	if got := readFile(t, filepath.Join(src, ".git", "fat", "objects", sha)); got != "fat content b\n" {
		t.Fatalf("common-dir object = %q", got)
	}
}

// TestCleanFailureFailsAdd checks the filter is required: a clean that cannot
// cache the object must stop git add, not let git store the full file.
func TestCleanFailureFailsAdd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only directory permissions are not enforced on Windows")
	}
	buildGitFat(t)
	src := newFatRepo(t)
	if got := sh(t, src, "git", "config", "--get", "filter.fat.required"); got != "true\n" {
		t.Fatalf("filter.fat.required = %q, want true", got)
	}
	objDir := filepath.Join(src, ".git", "fat", "objects")
	if err := os.Chmod(objDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(objDir, 0o755) })
	writeFile(t, filepath.Join(src, "b.fat"), "fat content b\n")
	cmd := exec.Command("git", "add", "b.fat")
	cmd.Dir = src
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("git add succeeded with an unwritable object dir:\n%s", out)
	}
	// status re-runs clean on stat-dirty files, so make the dir writable first.
	if err := os.Chmod(objDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if st := sh(t, src, "git", "status", "--porcelain"); st != "?? b.fat\n" {
		t.Fatalf("status = %q, want b.fat untracked", st)
	}
}

// TestAutoInitKeepsGlobalFilter matches Python, whose config check read every
// level: a globally configured fat filter must not be overridden locally.
func TestAutoInitKeepsGlobalFilter(t *testing.T) {
	buildGitFat(t)
	repo := filepath.Join(t.TempDir(), "repo")
	sh(t, "", "git", "init", "-q", repo)
	sh(t, "", "git", "config", "--global", "filter.fat.clean", "wrapper filter-clean")
	cmd := exec.Command("git-fat", "filter-clean")
	cmd.Dir = repo
	cmd.Stdin = strings.NewReader("x")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("filter-clean: %v\n%s", err, out)
	}
	if got := sh(t, repo, "git", "config", "--get", "filter.fat.clean"); got != "wrapper filter-clean\n" {
		t.Fatalf("filter.fat.clean = %q, want the global value", got)
	}
}

func TestNotInRepo(t *testing.T) {
	buildGitFat(t)
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	cmd := exec.Command("git-fat", "filter-clean")
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("exit = %v, want status 1", err)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "not in a git repository") {
		t.Fatalf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}

// TestStoreCommandWithoutConfig matches Python's error for a repo without
// .gitfat.
func TestStoreCommandWithoutConfig(t *testing.T) {
	buildGitFat(t)
	src := newFatRepo(t)
	cmd := exec.Command("git-fat", "push")
	cmd.Dir = src
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 || !strings.Contains(string(out), "git-fat: no valid fat config exists") {
		t.Fatalf("push without .gitfat: %v\n%s", err, out)
	}
}
