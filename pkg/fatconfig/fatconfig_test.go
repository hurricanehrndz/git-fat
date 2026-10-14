package fatconfig_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hurricanehrndz/git-fat/pkg/fatconfig"
)

// sampleconf is tests/conftest.py's sampleconf, the shape existing repos use.
const sampleconf = `
[s3]
bucket = 's3://fatstore'
endpoint = 'http://127.0.0.1:9000'
[s3.xpushargs]
ACL = 'bucket-owner-full-control'
[s3.smudgestore]
bucket = 's3://munkirepo'
endpoint = 'http://127.0.0.1:9000'
[s3.smudgestore.xpushargs]
ACL = 'bucket-owner-full-control'
`

// load writes content as a .gitfat in a temp dir and loads it.
func load(t *testing.T, content string) (*fatconfig.Store, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fatconfig.FileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return fatconfig.Load(dir)
}

func TestLoadSampleconf(t *testing.T) {
	got, err := load(t, sampleconf)
	if err != nil {
		t.Fatal(err)
	}
	acl := fatconfig.PushArgs{ACL: "bucket-owner-full-control"}
	want := &fatconfig.Store{
		Bucket:   "fatstore",
		Endpoint: "http://127.0.0.1:9000",
		PushArgs: acl,
		Smudge: &fatconfig.Store{
			Bucket:   "munkirepo",
			Endpoint: "http://127.0.0.1:9000",
			PushArgs: acl,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load = %+v, want %+v", got, want)
	}
}

// TestLoadAllPushArgs checks every boto3 ExtraArgs name reaches its field; a
// silently dropped arg would change what lands in the bucket.
func TestLoadAllPushArgs(t *testing.T) {
	got, err := load(t, `
[s3]
bucket = 'b'
[s3.xpushargs]
ACL = 'private'
CacheControl = 'no-cache'
ChecksumAlgorithm = 'SHA256'
ContentDisposition = 'attachment'
ContentEncoding = 'gzip'
ContentLanguage = 'en'
ContentType = 'application/octet-stream'
ExpectedBucketOwner = '123456789012'
Expires = 2030-01-02T03:04:05Z
GrantFullControl = 'id=full'
GrantRead = 'id=read'
GrantReadACP = 'id=readacp'
GrantWriteACP = 'id=writeacp'
ObjectLockLegalHoldStatus = 'ON'
ObjectLockMode = 'GOVERNANCE'
ObjectLockRetainUntilDate = '2031-01-02T03:04:05+01:00'
RequestPayer = 'requester'
ServerSideEncryption = 'aws:kms'
StorageClass = 'STANDARD_IA'
SSECustomerAlgorithm = 'AES256'
SSECustomerKey = 'key'
SSECustomerKeyMD5 = 'md5'
SSEKMSKeyId = 'key-1'
SSEKMSEncryptionContext = 'ctx'
Tagging = 'a=b'
WebsiteRedirectLocation = '/elsewhere'
[s3.xpushargs.Metadata]
owner = 'cpe'
`)
	if err != nil {
		t.Fatal(err)
	}
	want := fatconfig.PushArgs{
		ACL: "private", CacheControl: "no-cache", ChecksumAlgorithm: "SHA256",
		ContentDisposition: "attachment", ContentEncoding: "gzip", ContentLanguage: "en",
		ContentType: "application/octet-stream", ExpectedBucketOwner: "123456789012",
		Expires:          fatconfig.Timestamp{Time: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)},
		GrantFullControl: "id=full", GrantRead: "id=read", GrantReadACP: "id=readacp", GrantWriteACP: "id=writeacp",
		Metadata:                  map[string]string{"owner": "cpe"},
		ObjectLockLegalHoldStatus: "ON", ObjectLockMode: "GOVERNANCE",
		ObjectLockRetainUntilDate: fatconfig.Timestamp{Time: time.Date(2031, 1, 2, 3, 4, 5, 0, time.FixedZone("", 3600))},
		RequestPayer:              "requester", ServerSideEncryption: "aws:kms", StorageClass: "STANDARD_IA",
		SSECustomerAlgorithm: "AES256", SSECustomerKey: "key", SSECustomerKeyMD5: "md5",
		SSEKMSKeyID: "key-1", SSEKMSEncryptionContext: "ctx", Tagging: "a=b", WebsiteRedirectLocation: "/elsewhere",
	}
	// Times compare by instant; DeepEqual would also compare zones.
	for _, d := range []struct{ got, want *fatconfig.Timestamp }{
		{&got.PushArgs.Expires, &want.Expires},
		{&got.PushArgs.ObjectLockRetainUntilDate, &want.ObjectLockRetainUntilDate},
	} {
		if !d.got.Equal(d.want.Time) {
			t.Fatalf("date = %v, want %v", d.got, d.want)
		}
		*d.got = *d.want
	}
	if !reflect.DeepEqual(got.PushArgs, want) {
		t.Fatalf("PushArgs = %+v, want %+v", got.PushArgs, want)
	}
}

// TestLoadPushArgDates covers the two date forms boto3 users write and
// rejects offset-less ones, whose meaning depends on the pushing machine.
func TestLoadPushArgDates(t *testing.T) {
	want := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct{ name, value, wantErr string }{
		{"toml datetime", "2030-01-02T03:04:05Z", ""},
		{"toml offset datetime", "2030-01-02T04:04:05+01:00", ""},
		{"rfc 3339 string", "'2030-01-02T03:04:05Z'", ""},
		{"toml local datetime", "2030-01-02T03:04:05", "needs a UTC offset"},
		{"toml local date", "2030-01-02", "needs a UTC offset"},
		{"non-rfc 3339 string", "'2030-01-02'", "RFC 3339"},
		{"wrong type", "1", "want a datetime"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := load(t, "[s3]\nbucket = 'b'\n[s3.xpushargs]\nExpires = "+c.value+"\n")
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("Load err = %v, want one containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !got.PushArgs.Expires.Equal(want) {
				t.Fatalf("Expires = %v, want %v", got.PushArgs.Expires, want)
			}
		})
	}
}

func TestLoadNormalisesBucketAndPrefix(t *testing.T) {
	cases := []struct{ bucket, prefix, wantBucket, wantPrefix string }{
		{"s3://b", "", "b", ""},
		{"b", "p", "b", "p/"},
		{"b", "p/", "b", "p/"},
		{"b", "a/b", "b", "a/b/"},
	}
	for _, c := range cases {
		got, err := load(t, "[s3]\nbucket = '"+c.bucket+"'\nprefix = '"+c.prefix+"'\n")
		if err != nil {
			t.Fatal(err)
		}
		if got.Bucket != c.wantBucket || got.Prefix != c.wantPrefix {
			t.Errorf("bucket %q prefix %q: got %q, %q; want %q, %q",
				c.bucket, c.prefix, got.Bucket, got.Prefix, c.wantBucket, c.wantPrefix)
		}
	}
}

// TestLoadErrors covers configs that must fail at load rather than at the
// first push, where the cause is much harder to see.
func TestLoadErrors(t *testing.T) {
	cases := []struct{ name, content, want string }{
		{"empty", "", "no store configured"},
		{"first table wins", "[rsync]\nremote = 'x'\n[s3]\nbucket = 'b'\n", `unsupported store type "rsync"`},
		{"missing bucket", "[s3]\nprefix = 'p'\n", "bucket is required"},
		{"missing smudge bucket", "[s3]\nbucket = 'b'\n[s3.smudgestore]\nprefix = 'p'\n", "s3.smudgestore: bucket is required"},
		{"unknown store key", "[s3]\nbucket = 'b'\nbuckett = 'c'\n", `unknown key "s3.buckett"`},
		{"unknown smudge key", "[s3]\nbucket = 'b'\n[s3.smudgestore]\nbucket = 'c'\nx = 1\n", `unknown key "s3.smudgestore.x"`},
		{"unknown xpushargs key", "[s3]\nbucket = 'b'\n[s3.xpushargs]\nNotAnExtraArg = 'x'\n", `unknown key "s3.xpushargs.NotAnExtraArg"`},
		// The decoder matches keys case-insensitively; Python and boto3 did not.
		{"wrong-case store key", "[s3]\nBucket = 'b'\n", `unknown key "s3.Bucket" (keys are case-sensitive; did you mean "bucket"?)`},
		{"wrong-case smudge key", "[s3]\nbucket = 'b'\n[s3.smudgestore]\nbucket = 'c'\nPrefix = 'p'\n", `unknown key "s3.smudgestore.Prefix"`},
		{"wrong-case table", "[s3]\nbucket = 'b'\n[s3.XPushArgs]\nACL = 'private'\n", `unknown key "s3.XPushArgs"`},
		{"wrong-case xpushargs key", "[s3]\nbucket = 'b'\n[s3.xpushargs]\nacl = 'private'\n", `did you mean "ACL"?`},
		// Both spellings would decode into Bucket, one winning arbitrarily.
		{"case collision", "[s3]\nbucket = 'a'\nBucket = 'b'\n", `unknown key "s3.Bucket"`},
		{"bad toml", "[s3\n", "toml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := load(t, c.content)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Load err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

// TestLoadMissingFile pins the error callers turn into Python's "No valid
// fat config exists" exit.
func TestLoadMissingFile(t *testing.T) {
	_, err := fatconfig.Load(t.TempDir())
	if !errors.Is(err, fatconfig.ErrNoConfig) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load err = %v, want ErrNoConfig and fs.ErrNotExist", err)
	}
}

func TestStaticCredentialsNeedsBoth(t *testing.T) {
	cases := []struct {
		id, secret string
		want       bool
	}{
		{"id", "secret", true},
		{"id", "", false},
		{"", "secret", false},
	}
	for _, c := range cases {
		s := fatconfig.Store{ID: c.id, Secret: c.secret}
		if got := s.StaticCredentials(); got != c.want {
			t.Errorf("id %q secret %q: StaticCredentials = %v, want %v", c.id, c.secret, got, c.want)
		}
	}
}
