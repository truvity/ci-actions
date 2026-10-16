package s3cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// S3Config names the bucket. Everything comes from the caller; this
// repository is public and carries none of it.
type S3Config struct {
	Bucket    string
	Region    string
	Endpoint  string // non-AWS stores only
	PathStyle bool
	// CredentialTimeout bounds the one credential lookup made up front.
	CredentialTimeout time.Duration
}

type s3Store struct {
	c      *s3.Client
	up     *manager.Uploader
	bucket string
}

// NewS3 builds a Store over the default AWS credential chain (the runner's
// pod identity, or the AWS_PROFILE its config names), and resolves credentials
// once, with a deadline, so a runner that has none is found out in seconds
// rather than on every request. ErrNoStore wraps that failure.
func NewS3(ctx context.Context, sc S3Config) (Store, error) {
	if sc.Bucket == "" {
		return nil, fmt.Errorf("%w: no bucket", ErrNoStore)
	}
	opts := []func(*config.LoadOptions) error{}
	if sc.Region != "" {
		opts = append(opts, config.WithRegion(sc.Region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoStore, err)
	}
	to := sc.CredentialTimeout
	if to == 0 {
		to = 20 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	if _, err := cfg.Credentials.Retrieve(cctx); err != nil {
		return nil, fmt.Errorf("%w: credentials: %v", ErrNoStore, err)
	}
	cl := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if sc.Endpoint != "" {
			o.BaseEndpoint = aws.String(sc.Endpoint)
		}
		o.UsePathStyle = sc.PathStyle
		o.RetryMaxAttempts = 3
	})
	return &s3Store{c: cl, bucket: sc.Bucket, up: manager.NewUploader(cl, func(u *manager.Uploader) {
		u.PartSize = 32 << 20
		u.Concurrency = 4
	})}, nil
}

func (s *s3Store) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	if err == nil {
		return true, nil
	}
	if notFound(err) {
		return false, nil
	}
	return false, err
}

func notFound(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NotFound", "NoSuchKey":
			return true
		}
	}
	var he interface{ HTTPStatusCode() int }
	if errors.As(err, &he) && he.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	return false
}

func (s *s3Store) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	p := s3.NewListObjectsV2Paginator(s.c, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &prefix})
	for p.HasMorePages() {
		pg, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range pg.Contents {
			ob := Object{Key: aws.ToString(o.Key)}
			if o.LastModified != nil {
				ob.LastModified = *o.LastModified
			}
			out = append(out, ob)
		}
	}
	return out, nil
}

func (s *s3Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	o, err := s.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return nil, err
	}
	return o.Body, nil
}

func (s *s3Store) Put(ctx context.Context, key string, r io.Reader) error {
	_, err := s.up.Upload(ctx, &s3.PutObjectInput{Bucket: &s.bucket, Key: &key, Body: r})
	return err
}
