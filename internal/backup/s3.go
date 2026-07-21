package backup

import (
	"context"
	"fmt"
	"os"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config configures uploading backup snapshots to an S3-compatible bucket.
// One integration covers AWS S3, Cloudflare R2, MinIO, Backblaze B2, and GCS
// (interoperability mode) — anything that speaks the S3 API.
type S3Config struct {
	// Endpoint is host[:port] with no scheme. Empty means AWS S3
	// (s3.<region>.amazonaws.com). Set it for R2/MinIO/B2/GCS-interop.
	Endpoint string
	Region   string // signing region; defaults to us-east-1
	Bucket   string // target bucket; empty disables S3 upload
	Prefix   string // optional object-key prefix, e.g. "accelero/"
	AccessKey string
	SecretKey string
	UseSSL    bool // HTTPS; must be true for AWS, often false for local MinIO
}

// Enabled reports whether an S3 destination is configured.
func (c S3Config) Enabled() bool { return c.Bucket != "" }

// UploadFile uploads localPath to the bucket at Prefix+objectName and returns
// the object key. Retention on the remote side is left to bucket lifecycle
// policies (the idiomatic S3 way) — this only uploads.
func (c S3Config) UploadFile(ctx context.Context, localPath, objectName string) (string, error) {
	region := c.Region
	if region == "" {
		region = "us-east-1"
	}
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = "s3." + region + ".amazonaws.com"
	}

	cl, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""),
		Secure: c.UseSSL,
		Region: region,
	})
	if err != nil {
		return "", fmt.Errorf("s3 client: %w", err)
	}

	f, err := os.Open(localPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}

	key := c.Prefix + objectName
	if _, err := cl.PutObject(ctx, c.Bucket, key, f, fi.Size(),
		minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
		return "", fmt.Errorf("s3 put %q to bucket %q: %w", key, c.Bucket, err)
	}
	return key, nil
}
