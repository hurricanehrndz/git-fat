package pktline_test

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hurricanehrndz/git-fat/v2/pkg/pktline"
)

// packetSizes walks raw pkt-line output and returns each packet's payload
// size, -1 for a flush, without going through the Reader under test.
func packetSizes(t *testing.T, raw []byte) []int {
	t.Helper()
	var sizes []int
	for len(raw) > 0 {
		n, err := strconv.ParseUint(string(raw[:4]), 16, 16)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			sizes = append(sizes, -1)
			raw = raw[4:]
			continue
		}
		sizes = append(sizes, int(n)-4)
		raw = raw[n:]
	}
	return sizes
}

func TestWriteSplitsContent(t *testing.T) {
	var buf bytes.Buffer
	w := pktline.NewWriter(&buf)
	content := bytes.Repeat([]byte("0123456789"), 15000) // 150000 bytes
	if n, err := w.Write(content); err != nil || n != len(content) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if _, err := w.Write(nil); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFlush(); err != nil {
		t.Fatal(err)
	}
	want := []int{pktline.MaxData, pktline.MaxData, 150000 - 2*pktline.MaxData, -1}
	if got := packetSizes(t, buf.Bytes()); !slices.Equal(got, want) {
		t.Fatalf("packet sizes = %v, want %v", got, want)
	}
	got, err := io.ReadAll(pktline.NewReader(&buf).Content())
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("read back %d bytes, %v; want %d", len(got), err, len(content))
	}
}

// TestReadList checks the trailing newline is optional, as the spec requires.
func TestReadList(t *testing.T) {
	r := pktline.NewReader(strings.NewReader("000ecommand=a\n000epathname=b0004" + "0000"))
	got, err := r.ReadList()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != "command=a|pathname=b|" {
		t.Fatalf("list = %q", got)
	}
	if _, err = r.ReadList(); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadList at end = %v, want io.EOF", err)
	}
}

func TestReadListTruncated(t *testing.T) {
	r := pktline.NewReader(strings.NewReader("000ecommand=a\n"))
	if _, err := r.ReadList(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestReadRejectsMalformed(t *testing.T) {
	for name, in := range map[string]string{
		"non-hex":       "00g1abc",
		"sign":          "+00a",
		"length 1":      "0001",
		"length 3":      "0003",
		"too long":      "fff1" + strings.Repeat("x", 0xfff1-4),
		"short header":  "00",
		"short payload": "000aabc",
	} {
		if _, _, err := pktline.NewReader(strings.NewReader(in)).ReadPacket(); err == nil {
			t.Errorf("%s: ReadPacket(%.10q) succeeded", name, in)
		}
	}
}

// TestContentErrorsAreSticky lets a caller drain after a failed read without
// reading past a broken packet into whatever follows.
func TestContentErrorsAreSticky(t *testing.T) {
	c := pktline.NewReader(strings.NewReader("0007abc0002" + "0000")).Content()
	if _, err := io.ReadAll(c); err == nil {
		t.Fatal("read past a length-2 packet")
	}
	if _, err := io.Copy(io.Discard, c); err == nil {
		t.Fatal("drain after a stream error succeeded")
	}
}

func TestContentTruncated(t *testing.T) {
	c := pktline.NewReader(strings.NewReader("0007abc")).Content()
	if _, err := io.ReadAll(c); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestWriteTextTooLong(t *testing.T) {
	if err := pktline.NewWriter(io.Discard).WriteText(strings.Repeat("x", pktline.MaxData)); err == nil {
		t.Fatal("WriteText accepted a line that does not fit with its newline")
	}
}
