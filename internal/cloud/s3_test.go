package cloud

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/minio/minio-go/v7"
)

func TestS3EncryptedRoundTrip(t *testing.T) {
	endpoint := os.Getenv("MOIRAI_TEST_S3")
	if endpoint == "" {
		t.Skip("set MOIRAI_TEST_S3 to isolated MinIO")
	}
	bucket := "test-" + randomID()
	s3, err := NewS3(endpoint, bucket, "moirai_test", "synthetic-storage-password", false)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = s3.Client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s3.Client.RemoveBucket(ctx, bucket) })
	blobs, err := EncryptBlobs(s3, bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	id := randomID()
	archive := archiveFixture(t)
	if err = blobs.Put(ctx, id, archive); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Delete(ctx, id) })
	encrypted, err := s3.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte("moirai.session")) {
		t.Fatal("plaintext stored")
	}
	restored, err := blobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, archive) {
		t.Fatal("roundtrip differs")
	}
	if err = blobs.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err = blobs.Get(ctx, id); err == nil {
		t.Fatal("deleted object remains")
	}
}
