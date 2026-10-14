// Command git-fat is a git clean/smudge filter that stores large files
// outside git: git keeps a 74-byte stub and the content lives in
// .git/fat/objects.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/hurricanehrndz/git-fat/v2/pkg/fatconfig"
	"github.com/hurricanehrndz/git-fat/v2/pkg/fatrepo"
	"github.com/hurricanehrndz/git-fat/v2/pkg/fatstore"
)

const usage = `usage:
  git-fat init                     configure the fat filters for this repo
  git-fat push                     upload the index's fat objects the store lacks
  git-fat pull (--all | files...)  download fat objects and restore their files (also -a)
  git-fat pull --base <ref>        pull fat files HEAD adds or changes vs ref
  git-fat check [files...]         check the index's fat objects are in the store
  git-fat check --base <ref>       check fat files staged as added or changed vs ref are in the store
  git-fat publish --base <ref>     upload fat files HEAD adds or changes vs ref to the smudgestore
  git-fat version                  print the git-fat version and exit (also -v, --version)

git runs these; you do not:
  git-fat filter-clean             read file content on stdin, cache it, write its stub to stdout
  git-fat filter-smudge            read a stub on stdin, write the cached content to stdout
  git-fat filter-process           serve git's long-running filter protocol on stdin and stdout`

// legacy maps each 1.x subcommand to its 2.x name. The old names still
// work, unlisted, so 1.x scripts and pre-commit configs keep running.
var legacy = map[string]string{
	"fscheck":       "check",
	"fscheck-new":   "check",
	"pull-new":      "pull",
	"fspublish-new": "publish",
}

// legacyDefaultRef is the ref the 1.x *-new commands compare against when
// given none.
const legacyDefaultRef = "master"

// version is the git-fat release version, managed by `go tool versionbump`
// (see versionbump.yaml).
const version = "2.0.0"

func main() {
	// Cancel on the first interrupt so an in-flight multipart upload is
	// aborted; stop then restores the default, so a second one kills.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	if err == nil {
		return
	}
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, "git-fat:", err)
	os.Exit(1)
}

// run executes one git-fat invocation. stdout carries only file data or the
// version; every message goes to stderr.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("missing subcommand\n" + usage)
	}
	sub, rest, err := fromLegacy(args[0], args[1:])
	if err != nil {
		return err
	}
	switch sub {
	case "version", "-v", "--version":
		_, err = fmt.Fprintln(stdout, version)
		return err
	case "init", "filter-clean", "filter-smudge", "filter-process":
	case "push", "pull", "check", "publish":
		return runStore(ctx, sub, rest, stderr)
	default:
		return fmt.Errorf("unknown subcommand %q\n%s", sub, usage)
	}
	fs := flag.NewFlagSet("git-fat "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err = fs.Parse(rest); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%s takes no arguments, got %q", sub, fs.Arg(0))
	}
	repo, err := fatrepo.Open(ctx, "", stderr)
	if err != nil {
		return err
	}
	repo.Verbose = os.Getenv("GIT_FAT_VERBOSE") != ""
	switch sub {
	case "init":
		if err = repo.Init(ctx); err != nil {
			return err
		}
		_, err = fmt.Fprintln(stderr, "git-fat: Configured clean and smudge filter")
		return err
	case "filter-clean":
		return repo.Clean(stdin, stdout)
	case "filter-process":
		return repo.Process(stdin, stdout)
	default:
		return repo.Smudge(stdin, stdout)
	}
}

// fromLegacy rewrites a 1.x subcommand and its arguments as their 2.x
// form: fscheck becomes check, and X-new [ref] becomes X --base ref.
func fromLegacy(sub string, args []string) (string, []string, error) {
	name, ok := legacy[sub]
	if !ok {
		return sub, args, nil
	}
	if sub == "fscheck" {
		return name, args, nil
	}
	if len(args) > 1 {
		return "", nil, fmt.Errorf("%s takes at most one ref, got %q", sub, args)
	}
	ref := legacyDefaultRef
	if len(args) == 1 {
		ref = args[0]
	}
	return name, []string{"--base", ref}, nil
}

// storeArgs are a store command's parsed arguments.
type storeArgs struct {
	all   bool
	files []string
	base  string
}

// parseStoreArgs parses a store command's arguments.
func parseStoreArgs(sub string, args []string, stderr io.Writer) (storeArgs, error) {
	var a storeArgs
	fs := flag.NewFlagSet("git-fat "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	if sub == "pull" {
		fs.BoolVar(&a.all, "all", false, "download and restore every fat file in the index")
		fs.BoolVar(&a.all, "a", false, "short for --all")
	}
	if sub != "push" {
		fs.StringVar(&a.base, "base", "", "only fat files added or changed vs this ref")
	}
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return a, err
	}
	a.files, err = storeFiles(sub, a, pos)
	return a, err
}

// storeFiles checks a store command's positional arguments against its
// flags and returns the files it names.
func storeFiles(sub string, a storeArgs, pos []string) ([]string, error) {
	switch {
	case sub == "push" || sub == "publish":
		if len(pos) != 0 {
			return nil, fmt.Errorf("%s takes no arguments, got %q", sub, pos[0])
		}
		if sub == "publish" && a.base == "" {
			return nil, errors.New("publish: --base is required")
		}
	case a.base != "":
		if a.all || len(pos) != 0 {
			return nil, fmt.Errorf("%s: --base cannot be combined with --all or files", sub)
		}
	case sub == "pull" && !a.all && len(pos) == 0:
		return nil, errors.New("pull: use --all, --base or list files")
	case !a.all:
		// As in Python, --all wins and any files given with it are ignored.
		return pos, nil
	}
	return nil, nil
}

// parseInterleaved parses args with fs, allowing flags after positional
// arguments, and returns the positional ones.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		// After "--" everything left is positional.
		if len(args) > len(rest) && args[len(args)-len(rest)-1] == "--" {
			return append(pos, rest...), nil
		}
		pos, args = append(pos, rest[0]), rest[1:]
	}
}

// runStore runs a command that talks to the store .gitfat describes.
func runStore(ctx context.Context, sub string, args []string, stderr io.Writer) error {
	a, err := parseStoreArgs(sub, args, stderr)
	if err != nil {
		return err
	}
	repo, err := fatrepo.Open(ctx, "", stderr)
	if err != nil {
		return err
	}
	repo.Verbose = os.Getenv("GIT_FAT_VERBOSE") != ""
	top, err := repo.Toplevel(ctx)
	if err != nil {
		return err
	}
	cfg, err := fatconfig.Load(top)
	if err != nil {
		return err
	}
	store, err := fatstore.New(ctx, cfg)
	if err != nil {
		return err
	}
	switch sub {
	case "push":
		return repo.Push(ctx, store)
	case "publish":
		return publish(ctx, repo, store, cfg, a.base)
	}
	objs, err := storeObjects(ctx, repo, sub, a)
	if err != nil {
		return err
	}
	if sub == "pull" {
		return repo.Pull(ctx, sub, store, objs)
	}
	return repo.Check(ctx, sub, store, objs)
}

// storeObjects returns the fat objects pull or check works on.
func storeObjects(ctx context.Context, repo *fatrepo.Repo, sub string, a storeArgs) ([]fatrepo.Object, error) {
	switch {
	case a.base != "":
		// check --base is a pre-commit hook: it checks what is about to
		// be committed, so it compares the index rather than HEAD.
		return repo.NewObjects(ctx, a.base, sub == "check")
	case len(a.files) == 0:
		// pull --all, or check of the whole index.
		return repo.IndexObjects(ctx)
	}
	paths, err := repo.ResolvePaths(ctx, sub, a.files)
	if err != nil {
		return nil, err
	}
	if sub == "pull" {
		return repo.HeadObjects(ctx, sub, paths)
	}
	objs, err := repo.IndexObjects(ctx)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, p := range paths {
		want[p] = true
	}
	var named []fatrepo.Object
	for _, o := range objs {
		if want[o.Path] {
			named = append(named, o)
		}
	}
	return named, nil
}

// publish runs publish against cfg's smudgestore.
func publish(ctx context.Context, repo *fatrepo.Repo, store *fatstore.Store, cfg *fatconfig.Store, ref string) error {
	if cfg.Smudge == nil {
		return errors.New("publish: no smudgestore configured in " + fatconfig.FileName)
	}
	smudge, err := fatstore.New(ctx, cfg.Smudge)
	if err != nil {
		return err
	}
	objs, err := repo.NewObjects(ctx, ref, false)
	if err != nil {
		return err
	}
	return repo.Publish(ctx, store, smudge, objs)
}
