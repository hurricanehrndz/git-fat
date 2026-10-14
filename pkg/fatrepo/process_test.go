package fatrepo_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hurricanehrndz/git-fat/v2/pkg/fatrepo"
	"github.com/hurricanehrndz/git-fat/v2/pkg/fatstub"
	"github.com/hurricanehrndz/git-fat/v2/pkg/pktline"
)

// client scripts what git sends, as segments that each end where git would
// wait for a reply. Process reads the script through a lockstep reader that
// refuses to start the next segment until Process has written more output,
// so a filter that reads ahead of git fails here instead of deadlocking git.
type client struct {
	in   bytes.Buffer
	cuts []int
	w    *pktline.Writer
}

func newClient() *client {
	c := &client{}
	c.w = pktline.NewWriter(&c.in)
	return c
}

// wait ends the current segment: git now waits for a reply.
func (c *client) wait() *client {
	c.cuts = append(c.cuts, c.in.Len())
	return c
}

// list sends lines as text packets and a flush.
func (c *client) list(lines ...string) *client {
	for _, l := range lines {
		_ = c.w.WriteText(l)
	}
	_ = c.w.WriteFlush()
	return c
}

// handshake sends git's handshake offering caps.
func (c *client) handshake(caps ...string) *client {
	return c.list("git-filter-client", "version=2").wait().list(caps...).wait()
}

// request sends one filter request with content.
func (c *client) request(command, pathname string, content []byte) *client {
	c.list("command="+command, "pathname="+pathname)
	_, _ = c.w.Write(content)
	_ = c.w.WriteFlush()
	return c.wait()
}

// run feeds the script to Process and returns its error and a reader over
// its output, after checking no output packet exceeds the protocol limit.
func (c *client) run(t *testing.T, r *fatrepo.Repo) (*pktline.Reader, error) {
	t.Helper()
	var out bytes.Buffer
	err := r.Process(&lockstep{data: c.in.Bytes(), cuts: c.cuts, out: &out}, &out)
	assertPacketLimit(t, out.Bytes())
	return pktline.NewReader(&out), err
}

var errReadAhead = errors.New("filter read the next segment before replying")

// lockstep serves data, but crossing a cut needs out to have grown since
// the previous cut. End of input is always allowed: git may close any time.
type lockstep struct {
	data       []byte
	pos        int
	cuts       []int
	next, seen int
	out        *bytes.Buffer
}

func (l *lockstep) Read(p []byte) (int, error) {
	if l.pos == len(l.data) {
		return 0, io.EOF
	}
	if l.next < len(l.cuts) && l.pos == l.cuts[l.next] {
		if l.out.Len() == l.seen {
			return 0, errReadAhead
		}
		l.seen = l.out.Len()
		l.next++
	}
	end := len(l.data)
	if l.next < len(l.cuts) {
		end = l.cuts[l.next]
	}
	n := copy(p, l.data[l.pos:end])
	l.pos += n
	return n, nil
}

// assertPacketLimit walks raw output by hand rather than trusting the Reader.
func assertPacketLimit(t *testing.T, raw []byte) {
	t.Helper()
	for len(raw) >= 4 {
		n, err := strconv.ParseUint(string(raw[:4]), 16, 16)
		if err != nil || n > 4+pktline.MaxData || n > uint64(len(raw)) {
			t.Fatalf("bad output packet header %q", raw[:4])
		}
		raw = raw[max(n, 4):]
	}
	if len(raw) != 0 {
		t.Fatalf("output ends in a partial packet %q", raw)
	}
}

// readList reads one list from the reply, failing the test on error.
func readList(t *testing.T, pr *pktline.Reader) []string {
	t.Helper()
	l, err := pr.ReadList()
	if err != nil {
		t.Fatalf("reading reply: %v", err)
	}
	return l
}

// expectHandshake checks the server's reply advertises exactly caps.
func expectHandshake(t *testing.T, pr *pktline.Reader, caps ...string) {
	t.Helper()
	if got := readList(t, pr); !slices.Equal(got, []string{"git-filter-server", "version=2"}) {
		t.Fatalf("welcome = %q", got)
	}
	if got := readList(t, pr); !slices.Equal(got, caps) {
		t.Fatalf("capabilities = %q, want %q", got, caps)
	}
}

// response reads one reply and returns its final status and content.
func response(t *testing.T, pr *pktline.Reader) (string, []byte) {
	t.Helper()
	status := readList(t, pr)
	if !slices.Equal(status, []string{"status=success"}) {
		return strings.Join(status, ","), nil
	}
	content, err := io.ReadAll(pr.Content())
	if err != nil {
		t.Fatalf("reading content: %v", err)
	}
	if final := readList(t, pr); final != nil {
		return strings.Join(final, ","), content
	}
	return "status=success", content
}

// expectEnd checks the reply has nothing after the last response.
func expectEnd(t *testing.T, pr *pktline.Reader) {
	t.Helper()
	if _, _, err := pr.ReadPacket(); !errors.Is(err, io.EOF) {
		t.Fatalf("extra output after last response: %v", err)
	}
}

var bothCaps = []string{"capability=clean", "capability=smudge"}

func TestProcessHandshakeOnly(t *testing.T) {
	r, _ := newRepo(t)
	c := newClient().list("git-filter-client", "version=1", "version=2", "version=3").wait().
		list("capability=clean", "capability=delay", "capability=smudge")
	pr, err := c.run(t, r)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	expectHandshake(t, pr, bothCaps...) // never delay
	expectEnd(t, pr)
}

func TestProcessHandshakeRejected(t *testing.T) {
	for name, c := range map[string]*client{
		"no version 2": newClient().list("git-filter-client", "version=1").wait().list(bothCaps...),
		"wrong client": newClient().list("git-filter-other", "version=2").wait().list(bothCaps...),
		"empty input":  newClient(),
		"no caps":      newClient().list("git-filter-client", "version=2"),
	} {
		r, _ := newRepo(t)
		if _, err := c.run(t, r); err == nil {
			t.Errorf("%s: Process succeeded", name)
		}
	}
}

func TestProcessAdvertisesOnlyOffered(t *testing.T) {
	r, _ := newRepo(t)
	pr, err := newClient().handshake("capability=smudge").run(t, r)
	if err != nil {
		t.Fatal(err)
	}
	expectHandshake(t, pr, "capability=smudge")
}

// TestProcessSession runs several requests through one process, including
// content that spans several packets in both directions.
func TestProcessSession(t *testing.T) {
	r, _ := newRepo(t)
	small := []byte("fat content a\n")
	big := bytes.Repeat([]byte("0123456789abcdef"), 10000) // 160000 bytes
	missing := fatstub.Encode(sha1hex([]byte("absent")), 6)
	smallStub := fatstub.Encode(sha1hex(small), int64(len(small)))
	bigStub := fatstub.Encode(sha1hex(big), int64(len(big)))
	c := newClient().handshake(bothCaps...).
		request("clean", "a.fat", small).
		request("clean", "big.fat", big).
		request("smudge", "a.fat", smallStub).
		request("smudge", "big.fat", bigStub).
		request("smudge", "missing.fat", missing).
		request("clean", "empty.fat", nil)
	pr, err := c.run(t, r)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	expectHandshake(t, pr, bothCaps...)
	for i, want := range [][]byte{smallStub, bigStub, small, big, missing, fatstub.Encode(sha1hex(nil), 0)} {
		status, got := response(t, pr)
		if status != "status=success" || !bytes.Equal(got, want) {
			t.Fatalf("response %d = %s, %d bytes; want success, %d bytes", i, status, len(got), len(want))
		}
	}
	expectEnd(t, pr)
}

// TestProcessFileErrorsContinue keeps one bad file from ending the session:
// git would otherwise restart the process for every following file.
func TestProcessFileErrorsContinue(t *testing.T) {
	r, stderr := newRepo(t)
	bad := []byte("fat content a\n")
	if err := os.Mkdir(filepath.Join(r.ObjDir, sha1hex(bad)), 0o755); err != nil {
		t.Fatal(err)
	}
	badStub := fatstub.Encode(sha1hex(bad), int64(len(bad)))
	good := []byte("fat content b\n")
	c := newClient().handshake(bothCaps...).
		request("clean", "bad.fat", bad).
		request("smudge", "bad-smudge.fat", badStub).
		request("bogus", "x.fat", bytes.Repeat([]byte("z"), 70000)).
		request("clean", "good.fat", good)
	pr, err := c.run(t, r)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	expectHandshake(t, pr, bothCaps...)
	for _, name := range []string{"bad.fat", "bad-smudge.fat", "x.fat"} {
		if status, _ := response(t, pr); status != "status=error" {
			t.Fatalf("%s status = %s, want status=error", name, status)
		}
		if !strings.Contains(stderr.String(), name) {
			t.Fatalf("stderr %q does not name %s", stderr, name)
		}
	}
	status, got := response(t, pr)
	if want := fatstub.Encode(sha1hex(good), int64(len(good))); status != "status=success" || !bytes.Equal(got, want) {
		t.Fatalf("good.fat = %s, %q", status, got)
	}
	expectEnd(t, pr)
}

func TestProcessEOFMidRequest(t *testing.T) {
	inList := newClient().handshake(bothCaps...)
	_ = inList.w.WriteText("command=clean")
	inContent := newClient().handshake(bothCaps...).list("command=clean", "pathname=a.fat")
	_, _ = inContent.w.Write([]byte("partial"))
	for name, c := range map[string]*client{"in list": inList, "in content": inContent} {
		r, _ := newRepo(t)
		if _, err := c.run(t, r); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("%s: Process = %v, want io.ErrUnexpectedEOF", name, err)
		}
	}
}

func TestProcessMalformedPacket(t *testing.T) {
	c := newClient().handshake(bothCaps...).list("command=clean", "pathname=a.fat")
	c.in.WriteString("0002")
	r, _ := newRepo(t)
	if _, err := c.run(t, r); err == nil {
		t.Fatal("Process accepted a length-2 packet in content")
	}
}

// TestProcessCleanErrorDrains fails a clean before it reads past its first
// block, so only draining the rest keeps the next request aligned.
func TestProcessCleanErrorDrains(t *testing.T) {
	r, stderr := newRepo(t)
	// CreateTemp in a regular file fails at once.
	r.ObjDir = filepath.Join(r.ObjDir, "not-a-dir")
	if err := os.WriteFile(r.ObjDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("x"), 2*pktline.MaxData+1000)
	plain := []byte("not a stub\n")
	c := newClient().handshake(bothCaps...).
		request("clean", "big.fat", big).
		request("smudge", "plain.fat", plain)
	pr, err := c.run(t, r)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	expectHandshake(t, pr, bothCaps...)
	if status, _ := response(t, pr); status != "status=error" {
		t.Fatalf("big.fat status = %s, want status=error", status)
	}
	if !strings.Contains(stderr.String(), "big.fat") {
		t.Fatalf("stderr %q does not name big.fat", stderr)
	}
	if status, got := response(t, pr); status != "status=success" || !bytes.Equal(got, plain) {
		t.Fatalf("plain.fat = %s, %q", status, got)
	}
	expectEnd(t, pr)
}
