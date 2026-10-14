// Package fatconfig reads a repository's .gitfat file: the TOML description
// of the remote store fat objects are pushed to and pulled from.
package fatconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// FileName is the config file's name at the worktree top level.
const FileName = ".gitfat"

// ErrNoConfig is returned by Load when the repository has no .gitfat. The
// wording matches the message the Python git-fat printed in that case.
var ErrNoConfig = errors.New("no valid fat config exists")

// Store configures one S3 store. Load has normalised it: Bucket carries no
// s3:// scheme and Prefix is empty or ends in "/".
type Store struct {
	Bucket string `toml:"bucket"`
	Prefix string `toml:"prefix"`
	// Endpoint is a custom S3-compatible endpoint; it implies path-style
	// addressing.
	Endpoint string `toml:"endpoint"`
	// ID and Secret are static credentials, used only when both are set.
	ID     string `toml:"id"`
	Secret string `toml:"secret"`
	Region string `toml:"region"`
	// PushArgs are applied to every upload.
	PushArgs PushArgs `toml:"xpushargs"`
	// Smudge is the optional store smudged files are published to.
	Smudge *Store `toml:"smudgestore"`
}

// PushArgs mirrors boto3's S3Transfer ALLOWED_UPLOAD_ARGS, the ExtraArgs
// Python forwarded from xpushargs, keyed by the boto3 names so existing
// .gitfat files keep working.
type PushArgs struct {
	ACL                       string            `toml:"ACL"`
	CacheControl              string            `toml:"CacheControl"`
	ChecksumAlgorithm         string            `toml:"ChecksumAlgorithm"`
	ContentDisposition        string            `toml:"ContentDisposition"`
	ContentEncoding           string            `toml:"ContentEncoding"`
	ContentLanguage           string            `toml:"ContentLanguage"`
	ContentType               string            `toml:"ContentType"`
	ExpectedBucketOwner       string            `toml:"ExpectedBucketOwner"`
	Expires                   Timestamp         `toml:"Expires"`
	GrantFullControl          string            `toml:"GrantFullControl"`
	GrantRead                 string            `toml:"GrantRead"`
	GrantReadACP              string            `toml:"GrantReadACP"`
	GrantWriteACP             string            `toml:"GrantWriteACP"`
	Metadata                  map[string]string `toml:"Metadata"`
	ObjectLockLegalHoldStatus string            `toml:"ObjectLockLegalHoldStatus"`
	ObjectLockMode            string            `toml:"ObjectLockMode"`
	ObjectLockRetainUntilDate Timestamp         `toml:"ObjectLockRetainUntilDate"`
	RequestPayer              string            `toml:"RequestPayer"`
	ServerSideEncryption      string            `toml:"ServerSideEncryption"`
	StorageClass              string            `toml:"StorageClass"`
	SSECustomerAlgorithm      string            `toml:"SSECustomerAlgorithm"`
	SSECustomerKey            string            `toml:"SSECustomerKey"`
	SSECustomerKeyMD5         string            `toml:"SSECustomerKeyMD5"`
	SSEKMSKeyID               string            `toml:"SSEKMSKeyId"`
	SSEKMSEncryptionContext   string            `toml:"SSEKMSEncryptionContext"`
	Tagging                   string            `toml:"Tagging"`
	WebsiteRedirectLocation   string            `toml:"WebsiteRedirectLocation"`
}

// Timestamp is a push-arg date: a TOML offset datetime or an RFC 3339
// string. The zero value means unset.
type Timestamp struct{ time.Time }

// UnmarshalTOML implements toml.Unmarshaler. Local datetimes, dates and
// times are rejected: without an offset the moment they name depends on the
// pushing machine. The decoder marks them only with zones named
// "<kind>-local", which is what is checked.
func (t *Timestamp) UnmarshalTOML(v any) error {
	switch v := v.(type) {
	case time.Time:
		if strings.HasSuffix(v.Location().String(), "-local") {
			return fmt.Errorf("datetime %s needs a UTC offset", v.Format("2006-01-02T15:04:05"))
		}
		t.Time = v
	case string:
		p, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return fmt.Errorf("want an RFC 3339 datetime: %w", err)
		}
		t.Time = p
	default:
		return fmt.Errorf("want a datetime or RFC 3339 string, got %T", v)
	}
	return nil
}

// StaticCredentials reports whether both ID and Secret are set. With only
// one of them the default credential chain is used, as in Python.
func (s *Store) StaticCredentials() bool {
	return s.ID != "" && s.Secret != ""
}

// file is the decoded .gitfat. Only s3 is supported, so only s3 is decoded;
// which table comes first is checked against the metadata.
type file struct {
	S3 *Store `toml:"s3"`
}

// Load reads dir's .gitfat and returns its store. A missing file returns an
// error matching both ErrNoConfig and fs.ErrNotExist.
func Load(dir string) (*Store, error) {
	path := filepath.Join(dir, FileName)
	var f file
	md, err := toml.DecodeFile(path, &f)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %w", ErrNoConfig, err)
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := check(md, f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f.S3, nil
}

// check applies the rules decoding cannot: the first table names the store
// type, as Python took the config's first key; unknown keys inside the store
// are errors so typos fail loudly (Python ignored them); and the stores'
// values are validated and normalised.
func check(md toml.MetaData, f file) error {
	keys := md.Keys()
	if len(keys) == 0 {
		return errors.New("no store configured")
	}
	if typ := keys[0][0]; typ != "s3" {
		return fmt.Errorf("unsupported store type %q", typ)
	}
	// Keys outside the s3 table are ignored, as in Python. Inside it,
	// spelling is checked against the metadata rather than with Undecoded:
	// the decoder matches keys case-insensitively, so "Bucket" would pass
	// and, beside "bucket", one of the two would win arbitrarily.
	for _, k := range keys {
		if k[0] == "s3" {
			if err := checkKey(k); err != nil {
				return err
			}
		}
	}
	if err := normalise(f.S3); err != nil {
		return fmt.Errorf("s3: %w", err)
	}
	if s := f.S3.Smudge; s != nil {
		if s.Smudge != nil {
			return errors.New("s3.smudgestore: nested smudgestore is not supported")
		}
		if err := normalise(s); err != nil {
			return fmt.Errorf("s3.smudgestore: %w", err)
		}
	}
	return nil
}

// storeKeys and pushArgsKeys are the exact key spellings Python and boto3
// accepted; both were case-sensitive.
var (
	storeKeys    = tomlKeys(reflect.TypeFor[Store]())
	pushArgsKeys = tomlKeys(reflect.TypeFor[PushArgs]())
)

// tomlKeys returns the toml tag names of struct type t's fields.
func tomlKeys(t reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for f := range t.Fields() {
		keys[f.Tag.Get("toml")] = true
	}
	return keys
}

// checkKey rejects k, a key under the s3 table, unless every segment down to
// the push args is spelled exactly as a known key. Metadata's keys are the
// user's own.
func checkKey(k toml.Key) error {
	p := k[1:]
	for len(p) > 0 && p[0] == "smudgestore" {
		p = p[1:]
	}
	if len(p) == 0 {
		return nil
	}
	if !storeKeys[p[0]] {
		return unknownKey(k, p[0], storeKeys)
	}
	if p[0] == "xpushargs" && len(p) > 1 && !pushArgsKeys[p[1]] {
		return unknownKey(k, p[1], pushArgsKeys)
	}
	return nil
}

// unknownKey reports k as unknown, suggesting the right spelling when name
// differs from a valid key only in case.
func unknownKey(k toml.Key, name string, valid map[string]bool) error {
	for v := range valid {
		if strings.EqualFold(v, name) {
			return fmt.Errorf("unknown key %q (keys are case-sensitive; did you mean %q?)", k.String(), v)
		}
	}
	return fmt.Errorf("unknown key %q", k.String())
}

// normalise validates s and rewrites Bucket and Prefix to their canonical
// forms, as Python's get_bucket_name and get_predictable_prefix did.
func normalise(s *Store) error {
	s.Bucket = strings.TrimPrefix(s.Bucket, "s3://")
	if s.Bucket == "" {
		return errors.New("bucket is required")
	}
	if s.Prefix != "" && !strings.HasSuffix(s.Prefix, "/") {
		s.Prefix += "/"
	}
	return nil
}
