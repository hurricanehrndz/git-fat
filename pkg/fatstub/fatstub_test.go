package fatstub_test

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/hurricanehrndz/git-fat/v2/pkg/fatstub"
)

// TestEncodeMatchesPythonGolden pins the stub bytes to ones written by the
// Python filter_clean: existing repos hold those stubs, so any drift would
// make every fat file look modified.
func TestEncodeMatchesPythonGolden(t *testing.T) {
	cases := []struct{ input, golden string }{
		{"testdata/a.fat", "testdata/a.fat.stub"},
		{"testdata/cool-ranch.webp", "testdata/cool-ranch.webp.stub"},
	}
	for _, c := range cases {
		content, err := os.ReadFile(c.input)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(c.golden)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha1.Sum(content)
		got := fatstub.Encode(hex.EncodeToString(sum[:]), int64(len(content)))
		if !bytes.Equal(got, want) {
			t.Errorf("Encode(%s) = %q, want %q", c.input, got, want)
		}
		if len(got) != fatstub.Len {
			t.Errorf("len = %d, want %d", len(got), fatstub.Len)
		}
	}
}

func TestIsStub(t *testing.T) {
	stub := fatstub.Encode(strings.Repeat("a", 40), 14)
	cases := []struct {
		name string
		b    []byte
		want bool
	}{
		{"stub", stub, true},
		{"73 bytes", stub[:73], false},
		{"75 bytes", append(bytes.Clone(stub), 'x'), false},
		{"wrong cookie", append([]byte("#$# git-fit"), stub[11:]...), false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		if got := fatstub.IsStub(c.b); got != c.want {
			t.Errorf("%s: IsStub = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDecodeRoundTrip(t *testing.T) {
	sha := "f17cc23d902436b2c06e682c48e2a4132274c8d0"
	gotSHA, gotSize, err := fatstub.Decode(fatstub.Encode(sha, 340776))
	if err != nil || gotSHA != sha || gotSize != 340776 {
		t.Fatalf("Decode = %q, %d, %v; want %q, 340776, nil", gotSHA, gotSize, err, sha)
	}
}

func TestDecodeMissingSizeIsZero(t *testing.T) {
	sha := strings.Repeat("0", 40)
	_, size, err := fatstub.Decode([]byte(fatstub.Cookie + " " + sha + "\n"))
	if err != nil || size != 0 {
		t.Fatalf("Decode = %d, %v; want 0, nil", size, err)
	}
}

// TestDecodeRejectsUnsafeDigest guards the object dir: the digest becomes a
// file name there, so it must never carry path components.
func TestDecodeRejectsUnsafeDigest(t *testing.T) {
	for _, sha := range []string{
		"../../../../../../../../../../../etc/passwd",
		strings.Repeat("g", 40),
		strings.Repeat("A", 40),
		strings.Repeat("a", 39),
	} {
		b := []byte(fatstub.Cookie + " " + sha + " 1\n")
		if _, _, err := fatstub.Decode(b); err == nil {
			t.Errorf("Decode accepted digest %q", sha)
		}
	}
}
