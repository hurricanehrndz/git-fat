// Package fatstore moves fat objects to and from the S3 store a .gitfat
// describes. It knows nothing about git: callers choose keys and verify
// content.
package fatstore

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	tmtypes "github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/hurricanehrndz/git-fat/pkg/fatconfig"
)

// S3API is the S3 surface the store needs. It is transfermanager's, which
// already covers listing and single-stream gets; tests substitute a fake.
type S3API interface {
	transfermanager.S3APIClient
}

// Store is one S3 bucket and prefix holding fat objects.
type Store struct {
	// Out receives DRYRUN messages. New sets it to os.Stdout.
	Out io.Writer

	client S3API
	tm     *transfermanager.Client
	cfg    fatconfig.Store
}

// New returns a Store for cfg backed by a real S3 client. Credentials come
// from the SDK's default chain (environment, shared config and profiles,
// SSO, IMDS), the same sources boto3 reads, unless cfg sets static ones.
// optFns tune the transfer manager, e.g. its multipart part size.
func New(ctx context.Context, cfg *fatconfig.Store, optFns ...func(*transfermanager.Options)) (*Store, error) {
	var loadOpts []func(*config.LoadOptions) error
	if cfg.StaticCredentials() {
		loadOpts = append(loadOpts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.ID, cfg.Secret, "")))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	region, err := resolveRegion(ctx, cfg.Region, awsCfg.Region, cfg.Endpoint, func(ctx context.Context) (string, error) {
		return bucketRegion(ctx, awsCfg, cfg.Bucket)
	})
	if err != nil {
		return nil, err
	}
	// The client verifies TLS certificates. Python passed verify=False, which
	// let anyone on the path impersonate the store; that is deliberately gone.
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.Region = region
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = true
		}
	})
	// transfermanager defaults to WhenSupported regardless of the client,
	// which would override AWS_REQUEST_CHECKSUM_CALCULATION=WHEN_REQUIRED and
	// send checksums custom endpoints may reject. Carry the loaded setting
	// over; Unset still resolves to transfermanager's default.
	checksums := func(o *transfermanager.Options) { o.RequestChecksumCalculation = awsCfg.RequestChecksumCalculation }
	return NewWithClient(client, cfg, append([]func(*transfermanager.Options){checksums}, optFns...)...), nil
}

// failTimeout bounds cleanup after a failed transfer, chiefly aborting a
// multipart upload. transfermanager's default of 0 reuses the caller's
// context, so on cancellation the abort never reaches S3 and the uploaded
// parts linger, billed, until a lifecycle rule removes them.
const failTimeout = 30 * time.Second

// NewWithClient returns a Store for cfg that talks to client, so tests can
// inject a fake. Region and endpoint settings in cfg are the client's
// business and are ignored here.
// optFns run after the store's defaults, so they can override them.
func NewWithClient(client S3API, cfg *fatconfig.Store, optFns ...func(*transfermanager.Options)) *Store {
	defaults := func(o *transfermanager.Options) { o.FailTimeout = failTimeout }
	return &Store{
		Out:    os.Stdout,
		client: client,
		tm:     transfermanager.New(client, append([]func(*transfermanager.Options){defaults}, optFns...)...),
		cfg:    *cfg,
	}
}

// List returns every key under the store's prefix, with the prefix stripped.
func (s *Store) List(ctx context.Context) (map[string]bool, error) {
	keys := map[string]bool{}
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.cfg.Bucket),
		Prefix: aws.String(s.cfg.Prefix),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing s3://%s/%s: %w", s.cfg.Bucket, s.cfg.Prefix, err)
		}
		for _, o := range page.Contents {
			keys[strings.TrimPrefix(aws.ToString(o.Key), s.cfg.Prefix)] = true
		}
	}
	return keys, nil
}

// Upload stores the file at localPath under key, prefixed, with the
// configured push args. An empty key means the file's base name. Large files
// go up as multipart uploads, so objects past S3's 5 GB single-PUT limit
// work. When DRYRUN is set in the environment it only reports what it would
// upload, as Python did.
func (s *Store) Upload(ctx context.Context, localPath, key string) error {
	if key == "" {
		key = filepath.Base(localPath)
	}
	key = s.cfg.Prefix + key
	if os.Getenv("DRYRUN") != "" {
		_, err := fmt.Fprintf(s.Out, "%s would have been uploaded to s3://%s/%s\n", localPath, s.cfg.Bucket, key)
		return err
	}
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	in := uploadInput(s.cfg.PushArgs)
	in.Bucket, in.Key, in.Body = aws.String(s.cfg.Bucket), aws.String(key), f
	if _, err := s.tm.UploadObject(ctx, in); err != nil {
		return fmt.Errorf("uploading %s to s3://%s/%s: %w", localPath, s.cfg.Bucket, key, err)
	}
	return nil
}

// Download streams the object under key, prefixed, to w and returns its
// LastModified time. It is a single GET: a broken stream fails the call
// rather than resuming, and callers verify the content anyway.
func (s *Store) Download(ctx context.Context, key string, w io.Writer) (time.Time, error) {
	key = s.cfg.Prefix + key
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("downloading s3://%s/%s: %w", s.cfg.Bucket, key, err)
	}
	defer func() { _ = out.Body.Close() }()
	if _, err := io.Copy(w, out.Body); err != nil {
		return time.Time{}, fmt.Errorf("downloading s3://%s/%s: %w", s.cfg.Bucket, key, err)
	}
	return aws.ToTime(out.LastModified), nil
}

// uploadInput maps push args onto an upload. Empty strings and zero times
// stay nil so S3 applies its own defaults.
func uploadInput(a fatconfig.PushArgs) *transfermanager.UploadObjectInput {
	return &transfermanager.UploadObjectInput{
		ACL:                       tmtypes.ObjectCannedACL(a.ACL),
		CacheControl:              optional(a.CacheControl),
		ChecksumAlgorithm:         tmtypes.ChecksumAlgorithm(a.ChecksumAlgorithm),
		ContentDisposition:        optional(a.ContentDisposition),
		ContentEncoding:           optional(a.ContentEncoding),
		ContentLanguage:           optional(a.ContentLanguage),
		ContentType:               optional(a.ContentType),
		ExpectedBucketOwner:       optional(a.ExpectedBucketOwner),
		Expires:                   optionalTime(a.Expires),
		GrantFullControl:          optional(a.GrantFullControl),
		GrantRead:                 optional(a.GrantRead),
		GrantReadACP:              optional(a.GrantReadACP),
		GrantWriteACP:             optional(a.GrantWriteACP),
		Metadata:                  a.Metadata,
		ObjectLockLegalHoldStatus: tmtypes.ObjectLockLegalHoldStatus(a.ObjectLockLegalHoldStatus),
		ObjectLockMode:            tmtypes.ObjectLockMode(a.ObjectLockMode),
		ObjectLockRetainUntilDate: optionalTime(a.ObjectLockRetainUntilDate),
		RequestPayer:              tmtypes.RequestPayer(a.RequestPayer),
		ServerSideEncryption:      tmtypes.ServerSideEncryption(a.ServerSideEncryption),
		StorageClass:              tmtypes.StorageClass(a.StorageClass),
		SSECustomerAlgorithm:      optional(a.SSECustomerAlgorithm),
		SSECustomerKey:            optional(a.SSECustomerKey),
		SSECustomerKeyMD5:         optional(a.SSECustomerKeyMD5),
		SSEKMSKeyID:               optional(a.SSEKMSKeyID),
		SSEKMSEncryptionContext:   optional(a.SSEKMSEncryptionContext),
		Tagging:                   optional(a.Tagging),
		WebsiteRedirectLocation:   optional(a.WebsiteRedirectLocation),
	}
}

// optionalTime returns nil for the zero time and a pointer to t otherwise.
func optionalTime(t fatconfig.Timestamp) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t.Time
}

// optional returns nil for "" and a pointer to s otherwise.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
