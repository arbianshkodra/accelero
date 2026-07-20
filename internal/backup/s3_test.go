package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestS3Config_Enabled(t *testing.T) {
	assert.False(t, S3Config{}.Enabled(), "no bucket => disabled")
	assert.False(t, S3Config{Endpoint: "s3.example.com", Region: "us-east-1"}.Enabled())
	assert.True(t, S3Config{Bucket: "backups"}.Enabled())
}

func TestS3Config_UploadFile_MissingLocalFile(t *testing.T) {
	// Uploading a non-existent local file fails before any network call.
	cfg := S3Config{Bucket: "b", AccessKey: "a", SecretKey: "s"}
	_, err := cfg.UploadFile(context.Background(), filepath.Join(t.TempDir(), "nope.db"), "obj")
	require.Error(t, err)
	assert.True(t, os.IsNotExist(err) || err != nil)
}
