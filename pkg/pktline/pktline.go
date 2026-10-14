// Package pktline reads and writes git's pkt-line framing, as used by the
// long-running filter process protocol: each packet is a 4-digit hex length
// (counting the header itself) followed by the payload, and "0000" is a flush
// packet that ends a list or a stream of content.
package pktline

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// MaxData is the largest payload a packet may carry: git's 65520-byte packet
// limit minus the 4-byte length header.
const MaxData = 65516

const headerLen = 4

// Reader reads packets from a stream.
type Reader struct {
	r   *bufio.Reader
	buf [headerLen + MaxData]byte
}

// NewReader returns a Reader over r. It buffers r, so r must not be read
// directly afterwards.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReader(r)}
}

// ReadPacket reads one packet. A flush packet returns flush true and no data.
// The data is valid only until the next read. End of input before any byte of
// the packet returns io.EOF; end of input inside a packet returns
// io.ErrUnexpectedEOF.
func (r *Reader) ReadPacket() (data []byte, flush bool, err error) {
	hdr := r.buf[:headerLen]
	if _, err = io.ReadFull(r.r, hdr); err != nil {
		return nil, false, err
	}
	n, err := strconv.ParseUint(string(hdr), 16, 16)
	if err != nil {
		return nil, false, fmt.Errorf("pktline: malformed length header %q", hdr)
	}
	if n == 0 {
		return nil, true, nil
	}
	// 1-3 are too short to hold the header; git uses 1 and 2 as delimiters
	// in other protocols, which the filter protocol never sends.
	if n < headerLen || n > uint64(len(r.buf)) {
		return nil, false, fmt.Errorf("pktline: invalid packet length %d", n)
	}
	data = r.buf[headerLen:n]
	if _, err = io.ReadFull(r.r, data); err != nil {
		return nil, false, unexpected(err)
	}
	return data, false, nil
}

// ReadList reads text packets up to a flush packet and returns them with any
// trailing newline removed. End of input before the first packet returns
// io.EOF, so callers can tell a closed stream from a truncated list.
func (r *Reader) ReadList() ([]string, error) {
	var lines []string
	for {
		data, flush, err := r.ReadPacket()
		if err != nil {
			if lines != nil {
				err = unexpected(err)
			}
			return nil, err
		}
		if flush {
			return lines, nil
		}
		lines = append(lines, strings.TrimSuffix(string(data), "\n"))
	}
}

// Content returns a reader over the payloads of the following packets. It
// returns io.EOF at the flush packet and io.ErrUnexpectedEOF if the stream
// ends first. Errors are sticky, so draining a reader after a failed read
// reports the stream error again instead of reading past it.
func (r *Reader) Content() io.Reader {
	return &contentReader{r: r}
}

type contentReader struct {
	r   *Reader
	buf []byte
	err error
}

func (c *contentReader) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		if c.err != nil {
			return 0, c.err
		}
		data, flush, err := c.r.ReadPacket()
		switch {
		case err != nil:
			c.err = unexpected(err)
		case flush:
			c.err = io.EOF
		default:
			c.buf = data
		}
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

// unexpected turns io.EOF into io.ErrUnexpectedEOF for reads that had to
// continue.
func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// Writer writes packets to a stream. It does no buffering of its own; wrap
// the destination in a bufio.Writer to batch small packets.
type Writer struct {
	w io.Writer
}

// NewWriter returns a Writer over w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

// Write sends p as content, split into packets of at most MaxData bytes.
// Empty p sends nothing, since git discourages empty packets.
func (w *Writer) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		chunk := p[:min(len(p), MaxData)]
		if err := w.packet(chunk); err != nil {
			return written, err
		}
		written += len(chunk)
		p = p[len(chunk):]
	}
	return written, nil
}

// WriteText sends s plus a newline as one packet.
func (w *Writer) WriteText(s string) error {
	if len(s) >= MaxData {
		return fmt.Errorf("pktline: text packet of %d bytes is too long", len(s))
	}
	return w.packet([]byte(s + "\n"))
}

// WriteFlush sends a flush packet.
func (w *Writer) WriteFlush() error {
	_, err := io.WriteString(w.w, "0000")
	return err
}

// packet sends data, which must be 1 to MaxData bytes, as one packet.
func (w *Writer) packet(data []byte) error {
	if _, err := fmt.Fprintf(w.w, "%04x", len(data)+headerLen); err != nil {
		return err
	}
	_, err := w.w.Write(data)
	return err
}
