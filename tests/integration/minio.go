//go:build integration
// +build integration

package integration

// MinIO side of the harness: a real uploads bucket for a test that keeps
// files (business/Attachment Store and everything over it, the docs' imports
// and files, a public form's visitor files). It was a helper of the docs'
// tests only (startMinio), so a test outside tests/integration could not keep
// a file at all.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// minioImage is the S3 server the tests store files in. MinIO no longer
// publishes images on Docker Hub or Quay (the compose files' minio/minio tag
// answers "pull access denied" since late 2025), so the tests run
// Chainguard's build of the same server, pinned by digest;
// CONTENT_IO_TEST_MINIO_IMAGE names another.
func minioImage() string {
	if img := os.Getenv("CONTENT_IO_TEST_MINIO_IMAGE"); img != "" {
		return img
	}
	return "cgr.dev/chainguard/minio@sha256:dc455cdd4db6e390b1b26514f15ff37681a777bf8dab41aac445a0259f751e56"
}

// SetupMinio starts MinIO for the test, points the app's MinIO client at it
// and makes bucket the uploads bucket (USER_UPLOAD_BUCKET_NAME), until the
// test ends.
func SetupMinio(t *testing.T, bucket string) *minio.Client {
	t.Helper()
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        minioImage(),
			Cmd:          []string{"server", "/data"},
			Env:          map[string]string{"MINIO_ROOT_USER": "onecamp", "MINIO_ROOT_PASSWORD": "onecamp-secret"},
			ExposedPorts: []string{"9000/tcp"},
			WaitingFor:   wait.ForHTTP("/minio/health/live").WithPort("9000/tcp").WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start minio: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	host, _ := c.Host(ctx)
	port, _ := c.MappedPort(ctx, "9000")
	client, err := minio.New(fmt.Sprintf("%s:%s", host, port.Port()), &minio.Options{
		Creds: credentials.NewStaticV4("onecamp", "onecamp-secret", ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	saved := minioInit.MinioClient
	minioInit.MinioClient = client
	t.Cleanup(func() { minioInit.MinioClient = saved })
	t.Setenv("USER_UPLOAD_BUCKET_NAME", bucket)
	if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	return client
}
