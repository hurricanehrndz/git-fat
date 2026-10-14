package fatrepo_test

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hurricanehrndz/git-fat/pkg/fatrepo"
	"github.com/hurricanehrndz/git-fat/pkg/fatstub"
)

// newRepo returns a Repo over an empty temp object dir, logging to stderr.
func newRepo(t *testing.T) (*fatrepo.Repo, *bytes.Buffer) {
	t.Helper()
	stderr := &bytes.Buffer{}
	return &fatrepo.Repo{ObjDir: t.TempDir(), Stderr: stderr}, stderr
}

func sha1hex(b []byte) string {
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])
}

// clean runs Clean on content and returns the stub it wrote.
func clean(t *testing.T, r *fatrepo.Repo, content []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := r.Clean(bytes.NewReader(content), &out); err != nil {
		t.Fatalf("Clean: %v", err)
	}
	return out.Bytes()
}

// assertOnlyObjects fails if ObjDir holds anything but the named objects,
// e.g. a leftover temp file.
func assertOnlyObjects(t *testing.T, dir string, shas ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != strings.Join(shas, ",") {
		t.Fatalf("object dir holds %v, want %v", names, shas)
	}
}

func TestCleanCachesContent(t *testing.T) {
	cases := map[string][]byte{
		"small":      []byte("fat content a\n"),
		"empty":      {},
		"multiblock": bytes.Repeat([]byte("0123456789abcdef"), 1000), // 16000 bytes
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			r, _ := newRepo(t)
			sha := sha1hex(content)
			stub := clean(t, r, content)
			if want := fatstub.Encode(sha, int64(len(content))); !bytes.Equal(stub, want) {
				t.Fatalf("stub = %q, want %q", stub, want)
			}
			got, err := os.ReadFile(filepath.Join(r.ObjDir, sha))
			if err != nil || !bytes.Equal(got, content) {
				t.Fatalf("cached object = %d bytes, %v; want the %d input bytes", len(got), err, len(content))
			}
			assertOnlyObjects(t, r.ObjDir, sha)
		})
	}
}

// TestCleanPassesStubThrough keeps a stub from being cleaned into a stub of
// a stub, which would lose the link to the real content.
func TestCleanPassesStubThrough(t *testing.T) {
	r, _ := newRepo(t)
	stub := clean(t, r, []byte("fat content a\n"))
	if again := clean(t, r, stub); !bytes.Equal(again, stub) {
		t.Fatalf("re-clean = %q, want %q", again, stub)
	}
	assertOnlyObjects(t, r.ObjDir, sha1hex([]byte("fat content a\n")))
}

func TestCleanExistingObjectLeavesNoTemp(t *testing.T) {
	r, _ := newRepo(t)
	content := []byte("fat content a\n")
	first := clean(t, r, content)
	if second := clean(t, r, content); !bytes.Equal(second, first) {
		t.Fatalf("second stub = %q, want %q", second, first)
	}
	assertOnlyObjects(t, r.ObjDir, sha1hex(content))
}

// TestCleanReplacesTruncatedObject keeps a damaged cache entry from being
// trusted: Clean must not discard its good copy in favour of a short file.
func TestCleanReplacesTruncatedObject(t *testing.T) {
	r, _ := newRepo(t)
	content := []byte("fat content a\n")
	obj := filepath.Join(r.ObjDir, sha1hex(content))
	if err := os.WriteFile(obj, content[:5], 0o444); err != nil {
		t.Fatal(err)
	}
	clean(t, r, content)
	if got, _ := os.ReadFile(obj); !bytes.Equal(got, content) {
		t.Fatalf("object = %q, want %q", got, content)
	}
	assertOnlyObjects(t, r.ObjDir, sha1hex(content))
}

// TestCleanFailsWhenObjectIsDirectory makes an unusable cache entry an error,
// so git (with the filter required) refuses to stage a stub nobody can smudge.
func TestCleanFailsWhenObjectIsDirectory(t *testing.T) {
	r, _ := newRepo(t)
	content := []byte("fat content a\n")
	if err := os.Mkdir(filepath.Join(r.ObjDir, sha1hex(content)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.Clean(bytes.NewReader(content), &bytes.Buffer{}); err == nil {
		t.Fatal("Clean succeeded with a directory in the object's place")
	}
	assertOnlyObjects(t, r.ObjDir, sha1hex(content))
}

// smudge runs Smudge on in and returns what it wrote.
func smudge(t *testing.T, r *fatrepo.Repo, in []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := r.Smudge(bytes.NewReader(in), &out); err != nil {
		t.Fatalf("Smudge: %v", err)
	}
	return out.Bytes()
}

func TestSmudgeRestoresCachedObject(t *testing.T) {
	r, stderr := newRepo(t)
	content := bytes.Repeat([]byte("x"), 10000)
	if got := smudge(t, r, clean(t, r, content)); !bytes.Equal(got, content) {
		t.Fatalf("smudge wrote %d bytes, want %d", len(got), len(content))
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
}

// TestSmudgeMissingObjectEchoesStub keeps checkout working before a pull:
// the working tree gets the stub, which cleans back to the same blob.
func TestSmudgeMissingObjectEchoesStub(t *testing.T) {
	r, _ := newRepo(t)
	stub := fatstub.Encode(sha1hex([]byte("absent")), 6)
	if got := smudge(t, r, stub); !bytes.Equal(got, stub) {
		t.Fatalf("smudge = %q, want stub %q", got, stub)
	}
}

// TestSmudgePassesNonStubThrough guards against the Python behaviour of
// writing nothing, which emptied files committed without the filter.
func TestSmudgePassesNonStubThrough(t *testing.T) {
	r, _ := newRepo(t)
	stub := fatstub.Encode(sha1hex([]byte("x")), 1)
	for name, in := range map[string][]byte{
		"short":           []byte("plain text\n"),
		"long":            bytes.Repeat([]byte("y"), 9000),
		"stub then extra": append(bytes.Clone(stub), "more"...),
		"empty":           {},
	} {
		if got := smudge(t, r, in); !bytes.Equal(got, in) {
			t.Errorf("%s: smudge wrote %d bytes, want the %d input bytes", name, len(got), len(in))
		}
	}
}

func TestSmudgeSizeMismatchWarns(t *testing.T) {
	r, stderr := newRepo(t) // not verbose: the warning must still print
	content := []byte("fat content a\n")
	sha := sha1hex(content)
	clean(t, r, content)
	got := smudge(t, r, fatstub.Encode(sha, 99))
	if !bytes.Equal(got, content) {
		t.Fatalf("smudge = %q, want %q", got, content)
	}
	if !strings.Contains(stderr.String(), "expected: 99, got: 14") {
		t.Fatalf("stderr = %q, want size warning", stderr)
	}
}

// TestSmudgeLeavesUnsafeDigestAlone never opens a path built from a hostile
// digest, and passes the stub through rather than failing a required filter.
func TestSmudgeLeavesUnsafeDigestAlone(t *testing.T) {
	r, stderr := newRepo(t)
	bad := []byte(fatstub.Cookie + " ../../../../../../../../../../../../../../../../etc/passwd 12\n")
	if len(bad) != fatstub.Len {
		t.Fatalf("test stub is %d bytes, want %d", len(bad), fatstub.Len)
	}
	if got := smudge(t, r, bad); !bytes.Equal(got, bad) {
		t.Fatalf("smudge = %q, want the stub unchanged", got)
	}
	if !strings.Contains(stderr.String(), "leaving the stub in place") {
		t.Fatalf("stderr = %q, want a warning", stderr)
	}
}
