package fatstore_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/hurricanehrndz/pinsync/pkg/s3test"

	"github.com/hurricanehrndz/git-fat/pkg/fatconfig"
	"github.com/hurricanehrndz/git-fat/pkg/fatstore"
)

// fakeS3 wraps s3test.Fake with what the store tests need beyond it: the
// full put inputs, a LastModified on gets, and multipart calls that fail, so
// a small upload that strays off the single-PUT path is caught.
type fakeS3 struct {
	*s3test.Fake
	puts []*s3.PutObjectInput
	mod  time.Time
}

var errMultipart = errors.New("fake: multipart not supported")

func newFake() *fakeS3 {
	return &fakeS3{Fake: s3test.NewFake(), mod: time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (f *fakeS3) PutObject(ctx context.Context, in *s3.PutObjectInput, o ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.puts = append(f.puts, in)
	return f.Fake.PutObject(ctx, in, o...)
}

func (f *fakeS3) GetObject(ctx context.Context, in *s3.GetObjectInput, o ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	out, err := f.Fake.GetObject(ctx, in, o...)
	if err == nil {
		out.LastModified = aws.Time(f.mod)
	}
	return out, err
}

func (f *fakeS3) UploadPart(context.Context, *s3.UploadPartInput, ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	return nil, errMultipart
}

func (f *fakeS3) CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	return nil, errMultipart
}

func (f *fakeS3) CompleteMultipartUpload(context.Context, *s3.CompleteMultipartUploadInput, ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	return nil, errMultipart
}

func (f *fakeS3) AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	return nil, errMultipart
}

// writeFile writes content to a temp file named name and returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestListStripsPrefixAndPaginates uses more keys than one 1000-key page so
// a store that read only the first page would miss objects and re-push them.
func TestListStripsPrefixAndPaginates(t *testing.T) {
	f := newFake()
	for i := range 1001 {
		f.Store(fmt.Sprintf("p/%04d", i), nil)
	}
	f.Store("other/x", nil)
	s := fatstore.NewWithClient(f, &fatconfig.Store{Bucket: "b", Prefix: "p/"})
	got, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1001 || !got["0000"] || !got["1000"] || got["other/x"] {
		t.Fatalf("List returned %d keys (0000 %v, 1000 %v, other/x %v); want 1001 prefixed keys stripped",
			len(got), got["0000"], got["1000"], got["other/x"])
	}
}

func TestUploadAppliesPrefixAndPushArgs(t *testing.T) {
	f := newFake()
	s := fatstore.NewWithClient(f, &fatconfig.Store{
		Bucket:   "b",
		Prefix:   "p/",
		PushArgs: fatconfig.PushArgs{ACL: "bucket-owner-full-control", ContentType: "text/plain", Metadata: map[string]string{"k": "v"}},
	})
	if err := s.Upload(context.Background(), writeFile(t, "local", "data"), "obj"); err != nil {
		t.Fatal(err)
	}
	if body, ok := f.Object("p/obj"); !ok || string(body) != "data" {
		t.Fatalf("p/obj = %q, %v; want the uploaded data", body, ok)
	}
	in := f.puts[0]
	if aws.ToString(in.Bucket) != "b" || in.ACL != types.ObjectCannedACLBucketOwnerFullControl ||
		aws.ToString(in.ContentType) != "text/plain" || in.Metadata["k"] != "v" {
		t.Fatalf("put input bucket %q ACL %q ContentType %q Metadata %v; want push args applied",
			aws.ToString(in.Bucket), in.ACL, aws.ToString(in.ContentType), in.Metadata)
	}
	if in.CacheControl != nil || in.StorageClass != "" {
		t.Fatalf("unset push args sent: CacheControl %v StorageClass %q", in.CacheControl, in.StorageClass)
	}
}

func TestUploadEmptyKeyUsesBaseName(t *testing.T) {
	f := newFake()
	s := fatstore.NewWithClient(f, &fatconfig.Store{Bucket: "b"})
	if err := s.Upload(context.Background(), writeFile(t, "pkg.dmg", "x"), ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Object("pkg.dmg"); !ok {
		t.Fatal("pkg.dmg not uploaded under its base name")
	}
}

func TestUploadDryRun(t *testing.T) {
	t.Setenv("DRYRUN", "1")
	f := newFake()
	s := fatstore.NewWithClient(f, &fatconfig.Store{Bucket: "b", Prefix: "p/"})
	var out bytes.Buffer
	s.Out = &out
	path := writeFile(t, "local", "x")
	if err := s.Upload(context.Background(), path, "obj"); err != nil {
		t.Fatal(err)
	}
	if len(f.puts) != 0 {
		t.Fatalf("DRYRUN made %d puts, want 0", len(f.puts))
	}
	if want := path + " would have been uploaded to s3://b/p/obj\n"; out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}

func TestDownload(t *testing.T) {
	f := newFake()
	f.Store("p/obj", []byte("content"))
	s := fatstore.NewWithClient(f, &fatconfig.Store{Bucket: "b", Prefix: "p/"})
	var buf bytes.Buffer
	mod, err := s.Download(context.Background(), "obj", &buf)
	if err != nil {
		t.Fatal(err)
	}
	if buf.String() != "content" || !mod.Equal(f.mod) {
		t.Fatalf("Download = %q, %v; want %q, %v", buf.String(), mod, "content", f.mod)
	}
}

func TestDownloadMissingKey(t *testing.T) {
	s := fatstore.NewWithClient(newFake(), &fatconfig.Store{Bucket: "b"})
	var nsk *types.NoSuchKey
	if _, err := s.Download(context.Background(), "nope", &bytes.Buffer{}); !errors.As(err, &nsk) {
		t.Fatalf("Download err = %v, want NoSuchKey", err)
	}
}

// TestUploadMapsEveryPushArg sets each push arg alone and checks it reaches
// the put input. The table must name every PushArgs field, so a new arg that
// fatstore forgets to map fails here instead of being silently dropped.
func TestUploadMapsEveryPushArg(t *testing.T) {
	date := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	ts := fatconfig.Timestamp{Time: date}
	cases := map[string]struct {
		args fatconfig.PushArgs
		ok   func(*s3.PutObjectInput) bool
	}{
		"ACL":                 {fatconfig.PushArgs{ACL: "private"}, func(in *s3.PutObjectInput) bool { return in.ACL == types.ObjectCannedACLPrivate }},
		"CacheControl":        {fatconfig.PushArgs{CacheControl: "v"}, func(in *s3.PutObjectInput) bool { return aws.ToString(in.CacheControl) == "v" }},
		"ChecksumAlgorithm":   {fatconfig.PushArgs{ChecksumAlgorithm: "SHA256"}, func(in *s3.PutObjectInput) bool { return in.ChecksumAlgorithm == types.ChecksumAlgorithmSha256 }},
		"ContentDisposition":  {fatconfig.PushArgs{ContentDisposition: "v"}, func(in *s3.PutObjectInput) bool { return aws.ToString(in.ContentDisposition) == "v" }},
		"ContentEncoding":     {fatconfig.PushArgs{ContentEncoding: "v"}, func(in *s3.PutObjectInput) bool { return aws.ToString(in.ContentEncoding) == "v" }},
		"ContentLanguage":     {fatconfig.PushArgs{ContentLanguage: "v"}, func(in *s3.PutObjectInput) bool { return aws.ToString(in.ContentLanguage) == "v" }},
		"ContentType":         {fatconfig.PushArgs{ContentType: "v"}, func(in *s3.PutObjectInput) bool { return aws.ToString(in.ContentType) == "v" }},
		"ExpectedBucketOwner": {fatconfig.PushArgs{ExpectedBucketOwner: "v"}, func(in *s3.PutObjectInput) bool { return aws.ToString(in.ExpectedBucketOwner) == "v" }},
		"Expires":             {fatconfig.PushArgs{Expires: ts}, func(in *s3.PutObjectInput) bool { return aws.ToTime(in.Expires).Equal(date) }},
		"GrantFullControl":    {fatconfig.PushArgs{GrantFullControl: "v"}, func(in *s3.PutObjectInput) bool { return aws.ToString(in.GrantFullControl) == "v" }},
		"GrantRead":           {fatconfig.PushArgs{GrantRead: "v"}, func(in *s3.PutObjectInput) bool { return aws.ToString(in.GrantRead) == "v" }},
		"GrantReadACP":        {fatconfig.PushArgs{GrantReadACP: "v"}, func(in *s3.PutObjectInput) bool { return aws.ToString(in.GrantReadACP) == "v" }},
		"GrantWriteACP":       {fatconfig.PushArgs{GrantWriteACP: "v"}, func(in *s3.PutObjectInput) bool { return aws.ToString(in.GrantWriteACP) == "v" }},
		"Metadata":            {fatconfig.PushArgs{Metadata: map[string]string{"k": "v"}}, func(in *s3.PutObjectInput) bool { return in.Metadata["k"] == "v" }},
		"ObjectLockLegalHoldStatus": {fatconfig.PushArgs{ObjectLockLegalHoldStatus: "ON"}, func(in *s3.PutObjectInput) bool {
			return in.ObjectLockLegalHoldStatus == types.ObjectLockLegalHoldStatusOn
		}},
		"ObjectLockMode":            {fatconfig.PushArgs{ObjectLockMode: "GOVERNANCE"}, func(in *s3.PutObjectInput) bool { return in.ObjectLockMode == types.ObjectLockModeGovernance }},
		"ObjectLockRetainUntilDate": {fatconfig.PushArgs{ObjectLockRetainUntilDate: ts}, func(in *s3.PutObjectInput) bool { return aws.ToTime(in.ObjectLockRetainUntilDate).Equal(date) }},
		"RequestPayer":              {fatconfig.PushArgs{RequestPayer: "requester"}, func(in *s3.PutObjectInput) bool { return in.RequestPayer == types.RequestPayerRequester }},
		"ServerSideEncryption":      {fatconfig.PushArgs{ServerSideEncryption: "aws:kms"}, func(in *s3.PutObjectInput) bool { return in.ServerSideEncryption == types.ServerSideEncryptionAwsKms }},
		"StorageClass":              {fatconfig.PushArgs{StorageClass: "STANDARD_IA"}, func(in *s3.PutObjectInput) bool { return in.StorageClass == types.StorageClassStandardIa }},
		"SSECustomerAlgorithm":      {fatconfig.PushArgs{SSECustomerAlgorithm: "v"}, func(in *s3.PutObjectInput) bool { return aws.ToString(in.SSECustomerAlgorithm) == "v" }},
		"SSECustomerKey":            {fatconfig.PushArgs{SSECustomerKey: "v"}, func(in *s3.PutObjectInput) bool { return aws.ToString(in.SSECustomerKey) == "v" }},
		"SSECustomerKeyMD5":         {fatconfig.PushArgs{SSECustomerKeyMD5: "v"}, func(in *s3.PutObjectInput) bool { return aws.ToString(in.SSECustomerKeyMD5) == "v" }},
		"SSEKMSKeyID":               {fatconfig.PushArgs{SSEKMSKeyID: "v"}, func(in *s3.PutObjectInput) bool { return aws.ToString(in.SSEKMSKeyId) == "v" }},
		"SSEKMSEncryptionContext":   {fatconfig.PushArgs{SSEKMSEncryptionContext: "v"}, func(in *s3.PutObjectInput) bool { return aws.ToString(in.SSEKMSEncryptionContext) == "v" }},
		"Tagging":                   {fatconfig.PushArgs{Tagging: "v"}, func(in *s3.PutObjectInput) bool { return aws.ToString(in.Tagging) == "v" }},
		"WebsiteRedirectLocation":   {fatconfig.PushArgs{WebsiteRedirectLocation: "v"}, func(in *s3.PutObjectInput) bool { return aws.ToString(in.WebsiteRedirectLocation) == "v" }},
	}
	for f := range reflect.TypeFor[fatconfig.PushArgs]().Fields() {
		if _, ok := cases[f.Name]; !ok {
			t.Errorf("PushArgs.%s has no case", f.Name)
		}
	}
	path := writeFile(t, "local", "data")
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			s := fatstore.NewWithClient(f, &fatconfig.Store{Bucket: "b", PushArgs: c.args})
			if err := s.Upload(context.Background(), path, "obj"); err != nil {
				t.Fatal(err)
			}
			if !c.ok(f.puts[0]) {
				t.Fatalf("put input %+v lacks %s", f.puts[0], name)
			}
		})
	}
}

// mpuFake accepts multipart uploads, cancels ctx once the first part is in,
// and fails later calls on a done context as a real client would. It records
// whether the abort got through, which leaves no orphaned parts behind.
type mpuFake struct {
	*fakeS3
	cancel  context.CancelFunc
	parts   int
	aborted bool
}

func (f *mpuFake) CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	return &s3.CreateMultipartUploadOutput{UploadId: aws.String("upload-1")}, nil
}

func (f *mpuFake) UploadPart(ctx context.Context, in *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.parts++
	f.cancel()
	return &s3.UploadPartOutput{ETag: aws.String(fmt.Sprint(aws.ToInt32(in.PartNumber)))}, nil
}

func (f *mpuFake) AbortMultipartUpload(ctx context.Context, _ *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.aborted = true
	return &s3.AbortMultipartUploadOutput{}, nil
}

// TestUploadCancelAbortsMultipart cancels mid-upload: the abort must run on
// a context of its own, or S3 keeps the uploaded parts.
func TestUploadCancelAbortsMultipart(t *testing.T) {
	const part = 5 << 20
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &mpuFake{fakeS3: newFake(), cancel: cancel}
	s := fatstore.NewWithClient(f, &fatconfig.Store{Bucket: "b"}, func(o *transfermanager.Options) {
		o.PartSizeBytes = part
		o.MultipartUploadThreshold = part
		o.Concurrency = 1
	})
	err := s.Upload(ctx, writeFile(t, "big", strings.Repeat("x", 3*part)), "obj")
	if f.parts != 1 || !f.aborted {
		t.Fatalf("uploaded %d parts, aborted %v; want 1 part then a successful abort", f.parts, f.aborted)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Upload err = %v, want context.Canceled", err)
	}
}

// TestNewHonoursChecksumCalculation checks AWS_REQUEST_CHECKSUM_CALCULATION
// reaches uploads; transfermanager would otherwise always add CRC32, which
// some S3-compatible endpoints reject. WHEN_SUPPORTED is the control that
// shows the server would see a checksum if one were sent.
func TestNewHonoursChecksumCalculation(t *testing.T) {
	for _, c := range []struct {
		mode string
		want bool
	}{{"WHEN_SUPPORTED", true}, {"WHEN_REQUIRED", false}} {
		t.Run(c.mode, func(t *testing.T) {
			var headers http.Header
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				headers = r.Header.Clone()
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("ETag", `"etag"`)
			}))
			defer srv.Close()
			t.Setenv("AWS_REQUEST_CHECKSUM_CALCULATION", c.mode)
			t.Setenv("AWS_PROFILE", "")
			t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
			t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
			s, err := fatstore.New(context.Background(), &fatconfig.Store{
				Bucket: "b", Endpoint: srv.URL, Region: "us-east-1", ID: "id", Secret: "secret",
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Upload(context.Background(), writeFile(t, "local", "data"), "obj"); err != nil {
				t.Fatal(err)
			}
			var sent []string
			for k, v := range headers {
				k = strings.ToLower(k)
				if strings.HasPrefix(k, "x-amz-checksum-") || k == "x-amz-sdk-checksum-algorithm" || k == "x-amz-trailer" ||
					(k == "content-encoding" && strings.Contains(strings.Join(v, ","), "aws-chunked")) {
					sent = append(sent, k)
				}
			}
			if got := len(sent) > 0; got != c.want {
				t.Fatalf("checksum headers %v; want checksums sent: %v", sent, c.want)
			}
		})
	}
}
