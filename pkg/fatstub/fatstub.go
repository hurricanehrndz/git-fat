// Package fatstub encodes and decodes git-fat stubs: the fixed 74-byte
// placeholder git stores in place of a fat file's content. The format is
// byte-compatible with stubs written by the Python git-fat.
package fatstub

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
)

// Cookie is the prefix every stub starts with.
const Cookie = "#$# git-fat"

// Len is the length of every encoded stub in bytes: the cookie, a space, a
// 40-char SHA1, a space, a 20-wide size, and a newline.
const Len = 74

// Encode returns the stub for content with the given SHA1 hex digest and size.
func Encode(sha string, size int64) []byte {
	return fmt.Appendf(nil, "%s %s %20d\n", Cookie, sha, size)
}

// IsStub reports whether b is exactly one stub: Len bytes starting with Cookie.
// Longer input that merely starts with the cookie is content, not a stub.
func IsStub(b []byte) bool {
	return len(b) == Len && bytes.HasPrefix(b, []byte(Cookie))
}

// Decode returns the SHA1 hex digest and size recorded in stub b. A missing
// size decodes as 0, as in the Python implementation. The digest must be 40
// lowercase hex chars because callers use it as a file name in the object
// directory; anything else (e.g. "../x") is rejected.
func Decode(b []byte) (sha string, size int64, err error) {
	rest, ok := bytes.CutPrefix(b, []byte(Cookie))
	if !ok {
		return "", 0, errors.New("fatstub: missing cookie")
	}
	fields := bytes.Fields(rest)
	if len(fields) == 0 {
		return "", 0, errors.New("fatstub: missing digest")
	}
	sha = string(fields[0])
	if !ValidSHA(sha) {
		return "", 0, fmt.Errorf("fatstub: invalid digest %q", sha)
	}
	if len(fields) > 1 {
		size, err = strconv.ParseInt(string(fields[1]), 10, 64)
		if err != nil || size < 0 {
			return "", 0, fmt.Errorf("fatstub: invalid size %q", fields[1])
		}
	}
	return sha, size, nil
}

// ValidSHA reports whether s is a 40-char lowercase hex SHA1 digest.
func ValidSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range []byte(s) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
