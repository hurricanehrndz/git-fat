package fatstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// fallbackRegion signs requests to custom endpoints when nothing else names
// a region; S3-compatible servers ignore it. boto3 used it everywhere.
const fallbackRegion = "us-east-1"

// resolveRegion picks the client region: .gitfat's; for a custom endpoint the
// default chain's, else us-east-1; on AWS the bucket's own as discover reports
// it, else the chain's when discovery fails. boto3 needed no discovery because
// it followed S3's region redirects; the Go SDK does not, so a wrong region
// fails every request. On AWS the bucket's region beats the chain's, which
// names one region for every bucket and service: AWS_REGION or a profile
// region would otherwise break each store in another region.
func resolveRegion(ctx context.Context, configured, chain, endpoint string, discover func(context.Context) (string, error)) (string, error) {
	switch {
	case configured != "":
		return configured, nil
	case endpoint != "" && chain != "":
		return chain, nil
	case endpoint != "":
		return fallbackRegion, nil
	}
	region, err := discover(ctx)
	if err != nil && chain != "" {
		return chain, nil
	}
	return region, err
}

// bucketRegion asks S3 which region holds bucket. The request is anonymous so
// it needs no credentials: S3 names the region in the x-amz-bucket-region
// header even when it answers 301 or 403.
func bucketRegion(ctx context.Context, cfg aws.Config, bucket string) (string, error) {
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.Region = fallbackRegion
		o.Credentials = aws.AnonymousCredentials{}
	})
	out, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	if err == nil && aws.ToString(out.BucketRegion) != "" {
		return *out.BucketRegion, nil
	}
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		if r := re.Response.Header.Get("X-Amz-Bucket-Region"); r != "" {
			return r, nil
		}
	}
	if err == nil {
		err = errors.New("no region in response")
	}
	return "", fmt.Errorf("finding the region of bucket %q (set region in .gitfat or AWS_REGION): %w", bucket, err)
}
