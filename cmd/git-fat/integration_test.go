//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/hurricanehrndz/pinsync/pkg/s3test"
)

// gitFatConf writes a .gitfat for the RustFS behind client, as the Python
// sampleconf did for MinIO: a fat store and a smudgestore, both with the ACL
// the sample pushes.
func gitFatConf(t *testing.T, client *s3.Client, bucket, smudgeBucket string) string {
	t.Helper()
	creds, err := client.Options().Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	endpoint := aws.ToString(client.Options().BaseEndpoint)
	store := func(table, b string) string {
		return fmt.Sprintf("[%s]\nbucket = 's3://%s'\nendpoint = '%s'\nid = '%s'\nsecret = '%s'\n[%s.xpushargs]\nACL = 'bucket-owner-full-control'\n",
			table, b, endpoint, creds.AccessKeyID, creds.SecretAccessKey, table)
	}
	return store("s3", bucket) + store("s3.smudgestore", smudgeBucket)
}

// keys lists bucket.
func keys(t *testing.T, client *s3.Client, bucket string) []string {
	t.Helper()
	out, err := client.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatal(err)
	}
	var ks []string
	for _, o := range out.Contents {
		ks = append(ks, aws.ToString(o.Key))
	}
	return ks
}

// gitFat runs git fat with args in dir and returns stdout, stderr and the
// exit code.
func gitFat(t *testing.T, dir string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"fat"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return stdout.String(), stderr.String(), 0
	case errors.As(err, &ee):
		return stdout.String(), stderr.String(), ee.ExitCode()
	}
	t.Fatalf("git fat %s: %v", strings.Join(args, " "), err)
	return "", "", 0
}

// mustGitFat runs git fat and fails unless it exits 0; it returns stderr.
func mustGitFat(t *testing.T, dir string, args ...string) string {
	t.Helper()
	_, stderr, code := gitFat(t, dir, nil, args...)
	if code != 0 {
		t.Fatalf("git fat %s: exit %d\n%s", strings.Join(args, " "), code, stderr)
	}
	return stderr
}

// isolateAWS keeps the developer's AWS setup out of the binary's config
// loading; the .gitfat carries static credentials.
func isolateAWS(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("DRYRUN", "")
}

// TestRustFSFlow is the 1.x Python test_cmdline.py flow against RustFS:
// push, clone and pull, fscheck and fscheck-new, pull-new, fspublish-new
// and a DRYRUN push.
func TestRustFSFlow(t *testing.T) {
	buildGitFat(t)
	isolateAWS(t)
	client := s3test.StartRustFS(t)
	bucket := s3test.CreateBucket(t, client, "fatstore")
	smudgeBucket := s3test.CreateBucket(t, client, "munkirepo")

	src := newFatRepo(t)
	sh(t, src, "git", "branch", "-M", "master")
	writeFile(t, filepath.Join(src, ".gitfat"), gitFatConf(t, client, bucket, smudgeBucket))
	writeFile(t, filepath.Join(src, "b.fat"), "fat content b\n")
	sh(t, src, "git", "add", ".gitfat", "b.fat")
	sh(t, src, "git", "commit", "-q", "-m", "add b.fat and .gitfat")

	if code := exitOf(t, src, "fscheck"); code != 1 {
		t.Fatalf("fscheck before push: exit %d, want 1", code)
	}
	mustGitFat(t, src, "push")
	if ks := keys(t, client, bucket); len(ks) != 2 {
		t.Fatalf("store after push = %v, want 2 objects", ks)
	}
	if stderr := mustGitFat(t, src, "push"); !strings.Contains(stderr, "git-fat push: nothing to push") {
		t.Fatalf("second push stderr = %q", stderr)
	}
	mustGitFat(t, src, "fscheck")
	mustGitFat(t, src, "fscheck", "a.fat")
	mustGitFat(t, src, "fscheck-new")

	clone := filepath.Join(t.TempDir(), "clone")
	sh(t, "", "git", "clone", "-q", src, clone)
	mustGitFat(t, clone, "init")
	if got := readFile(t, filepath.Join(clone, "a.fat")); got == "fat content a\n" {
		t.Fatal("clone a.fat is full content before pull")
	}
	mustGitFat(t, clone, "pull", "a.fat")
	if got := readFile(t, filepath.Join(clone, "a.fat")); got != "fat content a\n" {
		t.Fatalf("a.fat after pull = %q", got)
	}
	// --all wins over the file named with it, as in Python.
	mustGitFat(t, clone, "pull", "a.fat", "--all")
	if got := readFile(t, filepath.Join(clone, "b.fat")); got != "fat content b\n" {
		t.Fatalf("b.fat after pull a.fat --all = %q", got)
	}
	if st := sh(t, clone, "git", "status", "--porcelain"); st != "" {
		t.Fatalf("clone status after pull:\n%s", st)
	}

	// New fat files on a branch: c.fat at the top, f.fat in a directory
	// with a space, as Python's publish test used.
	sh(t, src, "git", "checkout", "-q", "-b", "more_fat")
	writeFile(t, filepath.Join(src, "c.fat"), "fat content c\n")
	if err := os.Mkdir(filepath.Join(src, "test dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(src, "test dir", "f.fat"), "fat content f\n")
	sh(t, src, "git", "add", ".")
	if _, stderr, code := gitFat(t, src, nil, "fscheck-new"); code != 1 || !strings.Contains(stderr, "c.fat not found on remote store") {
		t.Fatalf("fscheck-new with staged c.fat: exit %d\n%s", code, stderr)
	}
	sh(t, src, "git", "commit", "-q", "-m", "more fat")

	stdout, stderr, code := gitFat(t, src, []string{"DRYRUN=1"}, "push")
	if code != 0 || strings.Count(stdout, "would have been uploaded") != 2 {
		t.Fatalf("DRYRUN push: exit %d\nstdout %q\nstderr %q", code, stdout, stderr)
	}
	if ks := keys(t, client, bucket); len(ks) != 2 {
		t.Fatalf("store after DRYRUN push = %v, want still 2 objects", ks)
	}
	mustGitFat(t, src, "push")
	mustGitFat(t, src, "fscheck-new", "master")

	sh(t, clone, "git", "fetch", "-q", "origin")
	sh(t, clone, "git", "checkout", "-q", "-b", "more_fat", "origin/more_fat")
	mustGitFat(t, clone, "pull-new")
	if got := readFile(t, filepath.Join(clone, "test dir", "f.fat")); got != "fat content f\n" {
		t.Fatalf("f.fat after pull-new = %q", got)
	}
	if st := sh(t, clone, "git", "status", "--porcelain"); st != "" {
		t.Fatalf("clone status after pull-new:\n%s", st)
	}

	// Publish from src with c.fat's object gone, so it is downloaded first.
	cSHA := strings.Fields(sh(t, src, "git", "cat-file", "-p", "HEAD:c.fat"))[2]
	if err := os.Remove(filepath.Join(src, ".git", "fat", "objects", cSHA)); err != nil {
		t.Fatal(err)
	}
	mustGitFat(t, src, "fspublish-new")
	if ks := keys(t, client, smudgeBucket); !slices.Equal(ks, []string{"c.fat", "test dir/f.fat"}) {
		t.Fatalf("smudgestore = %v", ks)
	}
	out, err := client.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String(smudgeBucket), Key: aws.String("test dir/f.fat")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Body.Close() }()
	if b, _ := io.ReadAll(out.Body); string(b) != "fat content f\n" {
		t.Fatalf("published f.fat = %q", b)
	}
}

func exitOf(t *testing.T, dir string, args ...string) int {
	t.Helper()
	_, _, code := gitFat(t, dir, nil, args...)
	return code
}
