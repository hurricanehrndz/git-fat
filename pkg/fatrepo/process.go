package fatrepo

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/hurricanehrndz/git-fat/v2/pkg/pktline"
)

// Process serves git's long-running filter process protocol (version 2) on
// in and out, so one git-fat process filters every file of a git command.
// It offers the clean and smudge capabilities, never delay.
//
// A failure to filter one file is reported to git as status=error and the
// session continues. A broken protocol stream ends the session with an
// error. End of input between requests means git is done and returns nil.
func (r *Repo) Process(in io.Reader, out io.Writer) error {
	pr := pktline.NewReader(in)
	bw := bufio.NewWriter(out)
	pw := pktline.NewWriter(bw)
	if err := handshake(pr, pw, bw); err != nil {
		return err
	}
	for {
		req, err := pr.ReadList()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = r.serve(req, pr, pw); err != nil {
			return err
		}
		// Each response must reach git before it sends the next request.
		if err = bw.Flush(); err != nil {
			return err
		}
	}
}

// handshake runs the welcome and version exchange, then the capability
// exchange. Git waits for the version reply before it sends its capabilities,
// so that reply is flushed before reading on.
func handshake(pr *pktline.Reader, pw *pktline.Writer, bw *bufio.Writer) error {
	hello, err := pr.ReadList()
	if err != nil {
		return fmt.Errorf("filter-process handshake: %w", unexpectedEOF(err))
	}
	if len(hello) == 0 || hello[0] != "git-filter-client" {
		return fmt.Errorf("filter-process handshake: unexpected welcome %q", hello)
	}
	if !slices.Contains(hello[1:], "version=2") {
		return fmt.Errorf("filter-process handshake: client does not offer version=2: %q", hello[1:])
	}
	if err = writeLists(pw, "git-filter-server", "version=2", ""); err != nil {
		return err
	}
	if err = bw.Flush(); err != nil {
		return err
	}
	caps, err := pr.ReadList()
	if err != nil {
		return fmt.Errorf("filter-process handshake: %w", unexpectedEOF(err))
	}
	var lines []string
	for _, c := range []string{"capability=clean", "capability=smudge"} {
		if slices.Contains(caps, c) {
			lines = append(lines, c)
		}
	}
	if err = writeLists(pw, append(lines, "")...); err != nil {
		return err
	}
	return bw.Flush()
}

// serve answers one request whose key=value list is req and whose content
// follows on pr. Only errors on the protocol stream are returned.
func (r *Repo) serve(req []string, pr *pktline.Reader, pw *pktline.Writer) error {
	var command, pathname string
	for _, kv := range req {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "command":
			command = v
		case "pathname":
			pathname = v
		}
	}
	content := pr.Content()
	switch command {
	case "clean":
		return r.serveClean(pathname, content, pw)
	case "smudge":
		return r.serveSmudge(pathname, content, pw)
	}
	if err := drain(content); err != nil {
		return err
	}
	r.logf(false, "filter-process: %s: unknown command %q", pathname, command)
	return writeLists(pw, "status=error", "")
}

// serveClean streams the content into Clean and replies with the stub.
func (r *Repo) serveClean(pathname string, content io.Reader, pw *pktline.Writer) error {
	var stub bytes.Buffer
	cerr := r.Clean(content, &stub)
	// Drain even on success so the stream is at the next request whatever
	// Clean left unread. The reader's errors are sticky, so a stream error
	// that made Clean fail surfaces here as fatal.
	if err := drain(content); err != nil {
		return err
	}
	if cerr != nil {
		r.logf(false, "filter-process: clean %s: %v", pathname, cerr)
		return writeLists(pw, "status=error", "")
	}
	if err := pw.WriteText("status=success"); err != nil {
		return err
	}
	if err := pw.WriteFlush(); err != nil {
		return err
	}
	if _, err := pw.Write(stub.Bytes()); err != nil {
		return err
	}
	return writeLists(pw, "", "")
}

// smudgeMem is how much smudge input serveSmudge keeps in memory before it
// spools the rest to a temp file.
const smudgeMem = 1 << 20

// serveSmudge replies with Smudge's output for the content.
//
// Git writes the whole request before it reads the response, so replying
// while input is still pending can deadlock both sides on full pipes. The
// input is therefore read in full first. Smudge input is normally a 74-byte
// stub, but a large file committed before the filter was enabled arrives
// whole, so only smudgeMem bytes stay in memory and the rest goes to disk.
func (r *Repo) serveSmudge(pathname string, content io.Reader, pw *pktline.Writer) error {
	in, cleanup, serr := r.smudgeInput(content)
	defer cleanup()
	// The reader's errors are sticky, so a stream error surfaces here as
	// fatal. Any error left is the temp file's and fails only this file.
	if err := drain(content); err != nil {
		return err
	}
	resp := &smudgeResponse{pw: pw}
	if serr == nil {
		serr = r.Smudge(in, resp)
		if resp.err != nil {
			return resp.err
		}
	}
	if serr == nil {
		if err := resp.start(); err != nil {
			return err
		}
		return writeLists(pw, "", "")
	}
	r.logf(false, "filter-process: smudge %s: %v", pathname, serr)
	if !resp.started {
		return writeLists(pw, "status=error", "")
	}
	// Content went out already; the protocol lets the status change after it.
	return writeLists(pw, "", "status=error", "")
}

// smudgeInput reads content and returns it as one reader: up to smudgeMem
// bytes from memory, the rest from a temp file in ObjDir. The returned
// cleanup closes and removes the temp file and must always run.
func (r *Repo) smudgeInput(content io.Reader) (io.Reader, func(), error) {
	nop := func() {}
	head, err := readUpTo(content, smudgeMem)
	if err != nil || len(head) < smudgeMem {
		return bytes.NewReader(head), nop, err
	}
	tmp, err := os.CreateTemp(r.ObjDir, "tmp-")
	if err != nil {
		return nil, nop, err
	}
	cleanup := func() {
		_ = tmp.Close() // only read back; a close error loses nothing
		_ = os.Remove(tmp.Name())
	}
	if _, err = io.Copy(tmp, content); err == nil {
		_, err = tmp.Seek(0, io.SeekStart)
	}
	if err != nil {
		cleanup()
		return nil, nop, err
	}
	return io.MultiReader(bytes.NewReader(head), tmp), cleanup, nil
}

// smudgeResponse sends status=success before the first content write, so a
// Smudge that fails before writing anything gets a plain error response.
// It records write errors, which are protocol errors, apart from Smudge's.
type smudgeResponse struct {
	pw      *pktline.Writer
	started bool
	err     error
}

func (s *smudgeResponse) Write(p []byte) (int, error) {
	if s.err == nil {
		s.err = s.start()
	}
	if s.err != nil {
		return 0, s.err
	}
	var n int
	n, s.err = s.pw.Write(p)
	return n, s.err
}

// start sends the status list once.
func (s *smudgeResponse) start() error {
	if s.started {
		return nil
	}
	s.started = true
	return writeLists(s.pw, "status=success", "")
}

// writeLists sends lines as text packets, with each empty line sent as a
// flush packet instead.
func writeLists(pw *pktline.Writer, lines ...string) error {
	for _, l := range lines {
		var err error
		if l == "" {
			err = pw.WriteFlush()
		} else {
			err = pw.WriteText(l)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// drain discards the rest of a request's content up to its flush packet.
func drain(content io.Reader) error {
	_, err := io.Copy(io.Discard, content)
	return err
}

// unexpectedEOF turns io.EOF into io.ErrUnexpectedEOF: during the handshake
// even a clean end of input is a protocol error.
func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
