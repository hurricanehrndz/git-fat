package fatstore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// TestResolveRegionOrder pins the precedence and that discovery, a network
// call, runs only on AWS without a .gitfat region. The chain region must not
// beat discovery on AWS: S3 answers 301 to a request signed for another
// region, and one AWS_REGION cannot match two stores in different regions.
func TestResolveRegionOrder(t *testing.T) {
	cases := []struct {
		name, configured, chain, endpoint string
		want                              string
		wantDiscover                      bool
	}{
		{"gitfat wins", "eu-west-1", "us-west-2", "http://x", "eu-west-1", false},
		{"gitfat wins on aws", "eu-west-1", "us-west-2", "", "eu-west-1", false},
		{"custom endpoint uses chain", "", "us-west-2", "http://x", "us-west-2", false},
		{"custom endpoint falls back", "", "", "http://x", "us-east-1", false},
		{"aws discovers", "", "", "", "ap-south-1", true},
		{"aws discovers over chain", "", "us-west-2", "", "ap-south-1", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			called := false
			got, err := resolveRegion(context.Background(), c.configured, c.chain, c.endpoint, func(context.Context) (string, error) {
				called = true
				return "ap-south-1", nil
			})
			if err != nil || got != c.want || called != c.wantDiscover {
				t.Fatalf("resolveRegion = %q, %v, discover called %v; want %q, nil, %v", got, err, called, c.want, c.wantDiscover)
			}
		})
	}
}

func TestResolveRegionDiscoverError(t *testing.T) {
	boom := errors.New("boom")
	_, err := resolveRegion(context.Background(), "", "", "", func(context.Context) (string, error) { return "", boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
}

func TestResolveRegionDiscoverErrorUsesChain(t *testing.T) {
	got, err := resolveRegion(context.Background(), "", "us-west-2", "", func(context.Context) (string, error) { return "", errors.New("boom") })
	if err != nil || got != "us-west-2" {
		t.Fatalf("resolveRegion = %q, %v; want us-west-2, nil", got, err)
	}
}

// TestBucketRegionReadsHeader serves the responses S3 gives a HeadBucket from
// the wrong region or without access; both carry the region header.
func TestBucketRegionReadsHeader(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusMovedPermanently, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Amz-Bucket-Region", "eu-central-1")
			w.WriteHeader(status)
		}))
		got, err := bucketRegion(context.Background(), testConfig(srv.URL), "b")
		srv.Close()
		if err != nil || got != "eu-central-1" {
			t.Errorf("status %d: bucketRegion = %q, %v; want eu-central-1", status, got, err)
		}
	}
}

func TestBucketRegionNoHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := bucketRegion(context.Background(), testConfig(srv.URL), "b")
	if err == nil || !strings.Contains(err.Error(), "set region in .gitfat") {
		t.Fatalf("err = %v, want one telling the user to set a region", err)
	}
}

// testConfig points S3 requests at url with no retries. The SDK addresses
// an IP endpoint path-style, so the bucket needs no DNS.
func testConfig(url string) aws.Config {
	return aws.Config{
		BaseEndpoint: aws.String(url),
		Retryer:      func() aws.Retryer { return aws.NopRetryer{} },
	}
}
