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
  git-fat push                     upload cached fat objects the store lacks
  git-fat pull (--all | files...)  download fat objects and restore their files (also -a)
  git-fat pull-new [ref]           pull fat objects new in HEAD vs ref (default master)
  git-fat fscheck [files...]       check the index's fat objects are in the store
  git-fat fscheck-new [ref]        check fat objects staged new vs ref (default master) are in the store
  git-fat fspublish-new [ref]      upload files new in HEAD vs ref (default master) to the smudgestore
  git-fat filter-clean             read file content on stdin, cache it, write its stub to stdout
  git-fat filter-smudge            read a stub on stdin, write the cached content to stdout
  git-fat filter-process           serve git's long-running filter protocol on stdin and stdout
  git-fat version                  print the git-fat version and exit (also -v, --version)`

// defaultRef is the ref the *-new commands compare against, as in Python.
const defaultRef = "master"

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
	sub := args[0]
	switch sub {
	case "version", "-v", "--version":
		_, err := fmt.Fprintln(stdout, version)
		return err
	case "init", "filter-clean", "filter-smudge", "filter-process":
	case "push", "pull", "pull-new", "fscheck", "fscheck-new", "fspublish-new":
		return runStore(ctx, sub, args[1:], stderr)
	default:
		return fmt.Errorf("unknown subcommand %q\n%s", sub, usage)
	}
	fs := flag.NewFlagSet("git-fat "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args[1:]); err != nil {
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

// storeArgs are a store command's parsed arguments.
type storeArgs struct {
	all   bool
	files []string
	ref   string
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
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return a, err
	}
	switch sub {
	case "push":
		if len(pos) != 0 {
			return a, fmt.Errorf("%s takes no arguments, got %q", sub, pos[0])
		}
	case "pull":
		if !a.all && len(pos) == 0 {
			return a, errors.New("pull: use --all or pass list files")
		}
		// As in Python, --all wins and any files given with it are ignored.
		if !a.all {
			a.files = pos
		}
	case "fscheck":
		a.files = pos
	default:
		if len(pos) > 1 {
			return a, fmt.Errorf("%s takes at most one ref, got %q", sub, pos)
		}
		a.ref = defaultRef
		if len(pos) == 1 {
			a.ref = pos[0]
		}
	}
	return a, nil
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
	case "fspublish-new":
		return publish(ctx, repo, store, cfg, a.ref)
	}
	objs, err := storeObjects(ctx, repo, sub, a)
	if err != nil {
		return err
	}
	if sub == "pull" || sub == "pull-new" {
		return repo.Pull(ctx, sub, store, objs)
	}
	return repo.Check(ctx, sub, store, objs)
}

// storeObjects returns the fat objects pull, pull-new, fscheck or
// fscheck-new works on.
func storeObjects(ctx context.Context, repo *fatrepo.Repo, sub string, a storeArgs) ([]fatrepo.Object, error) {
	switch {
	case sub == "pull-new":
		return repo.NewObjects(ctx, a.ref, false)
	case sub == "fscheck-new":
		// A pre-commit hook: check what is about to be committed.
		return repo.NewObjects(ctx, a.ref, true)
	case len(a.files) == 0:
		// pull --all, or fscheck of the whole index.
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

// publish runs fspublish-new against cfg's smudgestore.
func publish(ctx context.Context, repo *fatrepo.Repo, store *fatstore.Store, cfg *fatconfig.Store, ref string) error {
	if cfg.Smudge == nil {
		return errors.New("fspublish-new: no smudgestore configured in " + fatconfig.FileName)
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
