//go:build integration

package fatstore_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/hurricanehrndz/pinsync/pkg/s3test"

	"github.com/hurricanehrndz/git-fat/v2/pkg/fatconfig"
	"github.com/hurricanehrndz/git-fat/v2/pkg/fatstore"
)

const partSize = 5 << 20 // S3's minimum part size

// newRustFSStore starts RustFS and builds a Store through New, as .gitfat
// would: custom endpoint, static credentials, no region. Uploads above
// partSize go multipart.
func newRustFSStore(t *testing.T, prefix string, args fatconfig.PushArgs) (*fatstore.Store, *s3.Client, *fatconfig.Store) {
	t.Helper()
	client := s3test.StartRustFS(t)
	bucket := s3test.CreateBucket(t, client, "fatstore")
	creds, err := client.Options().Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	// Isolate New from the developer's AWS setup so the region falls back
	// to us-east-1 for the custom endpoint.
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	cfg := &fatconfig.Store{
		Bucket:   bucket,
		Prefix:   prefix,
		Endpoint: aws.ToString(client.Options().BaseEndpoint),
		ID:       creds.AccessKeyID,
		Secret:   creds.SecretAccessKey,
		PushArgs: args,
	}
	s, err := fatstore.New(context.Background(), cfg, func(o *transfermanager.Options) {
		o.PartSizeBytes = partSize
		o.MultipartUploadThreshold = partSize
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, client, cfg
}

// roundTrip uploads content as key and checks List and Download see it.
func roundTrip(t *testing.T, s *fatstore.Store, key string, content []byte) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "obj")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Upload(ctx, path, key); err != nil {
		t.Fatal(err)
	}
	keys, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || !keys[key] {
		t.Fatalf("List = %v, want only %q", keys, key)
	}
	var buf bytes.Buffer
	mod, err := s.Download(ctx, key, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), content) || mod.IsZero() {
		t.Fatalf("Download = %d bytes, mod %v; want the %d uploaded bytes and a LastModified", buf.Len(), mod, len(content))
	}
}

func TestRustFSRoundTrip(t *testing.T) {
	for _, prefix := range []string{"", "fat/"} {
		t.Run("prefix="+prefix, func(t *testing.T) {
			s, client, _ := newRustFSStore(t, prefix, fatconfig.PushArgs{})
			roundTrip(t, s, "obj", []byte("fat content\n"))
			// The key landed where the prefix says, not just where List looks.
			if _, err := client.HeadObject(context.Background(), &s3.HeadObjectInput{
				Bucket: aws.String("fatstore"), Key: aws.String(prefix + "obj"),
			}); err != nil {
				t.Fatalf("HeadObject %sobj: %v", prefix, err)
			}
		})
	}
}

// TestRustFSMultipart sends an object over the part size, the path multi-GB
// packages take, and checks it reassembles byte for byte.
func TestRustFSMultipart(t *testing.T) {
	s, client, _ := newRustFSStore(t, "", fatconfig.PushArgs{})
	content := make([]byte, 12<<20)
	_, _ = rand.Read(content)
	roundTrip(t, s, "big", content)
	out, err := client.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String("fatstore"), Key: aws.String("big"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// A multipart ETag ends in -<parts>; a single PUT's is a plain MD5.
	if etag := aws.ToString(out.ETag); !strings.HasSuffix(strings.Trim(etag, `"`), "-3") {
		t.Fatalf("ETag = %s, want a 3-part multipart ETag", etag)
	}
}

// TestRustFSPushArgs checks the sampleconf ACL is accepted and push args reach
// the stored object.
func TestRustFSPushArgs(t *testing.T) {
	s, client, _ := newRustFSStore(t, "", fatconfig.PushArgs{
		ACL:         "bucket-owner-full-control",
		ContentType: "application/x-fat",
	})
	roundTrip(t, s, "obj", []byte("x"))
	out, err := client.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String("fatstore"), Key: aws.String("obj"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := aws.ToString(out.ContentType); got != "application/x-fat" {
		t.Fatalf("ContentType = %q, want application/x-fat", got)
	}
}
