package minioInit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// The storage check right after a start. The API reaches MinIO by its public
// name, through Traefik, and for a moment after the stack starts that name may
// not route yet: on the demo's deploy of 17 Oct 2026 the first readiness check
// read "FAIL storage", and the three after it read ok. A look that fails is
// tried again, briefly and within the check's own deadline, and only the last
// answer counts.

// answers stands in for MinIO: the i-th look gets the i-th answer, and the
// last answer repeats.
type answers struct {
	got   []answer
	looks int
}

type answer struct {
	exists bool
	err    error
}

func (a *answers) look(context.Context, string) (bool, error) {
	i := a.looks
	if i >= len(a.got) {
		i = len(a.got) - 1
	}
	a.looks++
	return a.got[i].exists, a.got[i].err
}

// withStorage puts a MinIO client (never dialled) and the given answers in
// place for one test, with short waits between tries.
func withStorage(t *testing.T, a *answers) {
	t.Helper()
	client, err := minio.New("127.0.0.1:1", &minio.Options{Creds: credentials.NewStaticV4("k", "s", "")})
	if err != nil {
		t.Fatal(err)
	}
	savedClient, savedLook, savedWaits := MinioClient, lookBucket, storageProbeWaits
	MinioClient, lookBucket, storageProbeWaits = client, a.look, []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { MinioClient, lookBucket, storageProbeWaits = savedClient, savedLook, savedWaits })
	t.Setenv("USER_UPLOAD_BUCKET_NAME", "onecamp-uploads")
}

func TestStorageThatAnswersOnASecondLookIsReady(t *testing.T) {
	a := &answers{got: []answer{{err: errors.New("dial tcp: connection refused")}, {exists: true}}}
	withStorage(t, a)
	if err := probeStorage(context.Background()); err != nil {
		t.Fatalf("storage that answered on the second look read as failed: %v", err)
	}
	if a.looks != 2 {
		t.Errorf("looked %d times, want 2", a.looks)
	}
}

// Traefik's 404 for a name it doesn't route yet reads as "no such bucket" to
// the S3 client, so a missing bucket is looked for again too.
func TestABucketThereAMomentLaterIsReady(t *testing.T) {
	a := &answers{got: []answer{{exists: false}, {exists: true}}}
	withStorage(t, a)
	if err := probeStorage(context.Background()); err != nil {
		t.Fatalf("a bucket there on the second look read as missing: %v", err)
	}
}

func TestABucketThatIsMissingStillFails(t *testing.T) {
	a := &answers{got: []answer{{exists: false}}}
	withStorage(t, a)
	err := probeStorage(context.Background())
	if err == nil || !strings.Contains(err.Error(), `bucket "onecamp-uploads" does not exist`) {
		t.Fatalf("a missing bucket read as %v", err)
	}
	if a.looks != 3 {
		t.Errorf("looked %d times, want 3 (one, and two more)", a.looks)
	}
}

func TestStorageThatIsDownStillFails(t *testing.T) {
	a := &answers{got: []answer{{err: errors.New("dial tcp: connection refused")}}}
	withStorage(t, a)
	err := probeStorage(context.Background())
	if err == nil || !strings.Contains(err.Error(), "could not reach MinIO") {
		t.Fatalf("storage that never answered read as %v", err)
	}
}

// The tries stay inside the check's deadline (readinessCheckTimeout): once it
// has passed, no more looks and no more waiting.
func TestTheChecksDeadlineEndsTheTries(t *testing.T) {
	a := &answers{got: []answer{{err: errors.New("dial tcp: i/o timeout")}}}
	withStorage(t, a)
	storageProbeWaits = []time.Duration{time.Hour, time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := probeStorage(ctx); err == nil {
		t.Fatal("storage that never answered read as ready")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the check took %v past its deadline", took)
	}
	if a.looks != 1 {
		t.Errorf("looked %d times after the deadline, want 1", a.looks)
	}
}
