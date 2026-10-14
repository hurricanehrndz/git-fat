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

	"github.com/hurricanehrndz/git-fat/pkg/fatrepo"
)

const usage = `usage:
  git-fat init            configure the fat filters for this repo
  git-fat filter-clean    read file content on stdin, cache it, write its stub to stdout
  git-fat filter-smudge   read a stub on stdin, write the cached content to stdout
  git-fat filter-process  serve git's long-running filter protocol on stdin and stdout
  git-fat version         print the git-fat version and exit (also -v, --version)`

// version is the git-fat release version.
const version = "2.0.0-dev"

func main() {
	err := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
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
