package storage

import (
	"fmt"
	"io"

	"arcusinvest/internal/config"
)

// S3 is a placeholder Storage driver for object storage (AWS S3 or an
// S3-compatible endpoint such as MinIO/R2). It is intentionally NOT wired up:
// storage.New returns a startup error for STORAGE_DRIVER=s3 rather than
// constructing this type, so no untested code path runs in production and the
// aws-sdk dependency is not pulled in.
//
// To finish this driver later:
//  1. Add the AWS SDK (github.com/aws/aws-sdk-go-v2 + service/s3) to go.mod.
//  2. Build a client in NewS3 from cfg.S3Region / cfg.S3Endpoint and the
//     access/secret keys (support a custom endpoint + path-style for MinIO/R2).
//  3. Save -> PutObject(bucket=cfg.S3Bucket, key, body=r, ContentLength=size,
//     ContentType=contentType).
//  4. Open -> GetObject(...).Body (an io.ReadCloser the caller streams+closes).
//  5. Flip storage.New's "s3" case to return NewS3(cfg).
//
// Keys stay identical to the local driver ("submissions/<uuid><ext>") so the
// swap is transparent to handlers.
type S3 struct {
	bucket    string
	region    string
	endpoint  string
	accessKey string
	secretKey string
	// TODO: hold the concrete *s3.Client here once the SDK is added.
}

// NewS3 is unused today (storage.New errors out before reaching it). It exists
// so this file compiles as a complete Storage implementation and documents the
// intended construction.
func NewS3(cfg *config.Config) (*S3, error) {
	return &S3{
		bucket:    cfg.S3Bucket,
		region:    cfg.S3Region,
		endpoint:  cfg.S3Endpoint,
		accessKey: cfg.S3AccessKey,
		secretKey: cfg.S3SecretKey,
	}, nil
}

func (s *S3) Save(key string, r io.Reader, size int64, contentType string) error {
	// TODO: s3.PutObject(bucket=s.bucket, key=key, body=r, contentLength=size,
	// contentType=contentType).
	return fmt.Errorf("s3 storage driver not yet implemented")
}

func (s *S3) Open(key string) (io.ReadCloser, error) {
	// TODO: return s3.GetObject(bucket=s.bucket, key=key).Body.
	return nil, fmt.Errorf("s3 storage driver not yet implemented")
}
