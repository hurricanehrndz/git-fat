// Package fatrepo binds git-fat to a git repository: it locates the fat object
// directory, configures the clean and smudge filters, and implements the
// filters themselves. All git access goes through the git CLI.
package fatrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Repo is a git repository's fat object store. Clean and Smudge need only
// ObjDir and Stderr, so tests may build a Repo literal without git.
type Repo struct {
	// Dir is the directory git commands run in; empty means the current one.
	Dir string
	// ObjDir holds cached fat objects, one file per SHA1 hex digest.
	ObjDir string
	// Stderr receives all messages; nil discards them. Stdout is never used
	// because it carries file data to git.
	Stderr io.Writer
	// Verbose enables informational messages; warnings print regardless.
	Verbose bool

	// top caches Toplevel.
	top string
}

// Open discovers the repository containing dir, creates its object directory,
// and configures the fat filters if they are not configured yet, mirroring
// the Python setup() that runs on every invocation.
//
// The object directory is <git-common-dir>/fat/objects rather than Python's
// <toplevel>/.git/fat/objects: the two agree in a normal repository, but only
// the former works in a linked worktree, where .git is a file.
func Open(ctx context.Context, dir string, stderr io.Writer) (*Repo, error) {
	out, err := git(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("not in a git repository: %w", err)
	}
	common := filepath.FromSlash(out)
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	r := &Repo{Dir: dir, ObjDir: filepath.Join(common, "fat", "objects"), Stderr: stderr}
	if err := os.MkdirAll(r.ObjDir, 0o755); err != nil {
		return nil, err
	}
	// Check the effective config, as Python's config_reader did, so a fat
	// filter configured globally is left alone.
	if _, err := git(ctx, dir, "config", "--get", "filter.fat.clean"); err != nil {
		if err := r.Init(ctx); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Init sets the fat clean and smudge filters in the repository's local config,
// overwriting any existing values. It also marks the filter required, which
// Python did not: otherwise git treats a failed clean as a pass-through and
// commits the full file instead of its stub.
//
// It sets the long-running process filter too, which git prefers so one
// process serves a whole command. The clean and smudge commands stay for tools
// that do not speak the process protocol, such as libgit2 and JGit.
func (r *Repo) Init(ctx context.Context) error {
	for _, kv := range [][2]string{
		{"filter.fat.clean", "git fat filter-clean"},
		{"filter.fat.smudge", "git fat filter-smudge"},
		{"filter.fat.process", "git fat filter-process"},
		{"filter.fat.required", "true"},
	} {
		if _, err := git(ctx, r.Dir, "config", "--local", kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// git runs git with args in dir and returns its trimmed stdout. A failure
// carries git's stderr.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitBytes(ctx, dir, nil, args...)
	return strings.TrimRight(string(out), "\r\n"), err
}

// gitBytes runs git with args in dir, feeding it stdin when non-nil, and
// returns its raw stdout, for -z and --batch output that must not be trimmed.
func gitBytes(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// Toplevel returns the absolute path of the worktree's top directory, where
// .gitfat lives and against which repo-relative paths resolve.
func (r *Repo) Toplevel(ctx context.Context) (string, error) {
	if r.top == "" {
		out, err := git(ctx, r.Dir, "rev-parse", "--show-toplevel")
		if err != nil {
			return "", err
		}
		r.top = filepath.FromSlash(out)
	}
	return r.top, nil
}

// logf prints a git-fat message to Stderr; verbose-only messages are dropped
// unless Verbose is set. A failed stderr write is ignored so it cannot fail a
// filter whose data went through fine.
func (r *Repo) logf(verboseOnly bool, format string, args ...any) {
	if r.Stderr == nil || (verboseOnly && !r.Verbose) {
		return
	}
	_, _ = fmt.Fprintf(r.Stderr, "git-fat "+format+"\n", args...)
}
