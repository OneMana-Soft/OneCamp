package business

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"

	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// An upload of a size not known ahead holds one small part in memory, not the
// 528 MB part minio-go sizes for a 5 TiB object when it isn't told: every
// file a Slack import brings goes up this way, two at a time.
func TestAnUploadOfUnknownSizeHoldsOneSmallPart(t *testing.T) {
	var mu sync.Mutex
	stored := map[string]string{}
	parts := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodPost && q.Has("uploads"):
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><UploadId>u1</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut && q.Get("uploadId") != "":
			body, _ := io.ReadAll(r.Body)
			parts[q.Get("partNumber")] = string(body)
			w.Header().Set("ETag", `"0123456789abcdef0123456789abcdef"`)
		case r.Method == http.MethodPost && q.Get("uploadId") != "":
			stored[r.URL.Path] = parts["1"]
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><CompleteMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><ETag>"0123456789abcdef0123456789abcdef"</ETag></CompleteMultipartUploadResult>`)
		default:
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer srv.Close()
	endpoint, _ := url.Parse(srv.URL)
	client, err := minio.New(endpoint.Host, &minio.Options{Creds: credentials.NewStaticV4("key", "secret", ""), Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	was := minioInit.MinioClient
	minioInit.MinioClient = client
	t.Cleanup(func() { minioInit.MinioClient = was })

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if _, _, err := SafeUploadToMinio(context.Background(), strings.NewReader("hello"), SafeUploadOptions{
		Bucket: "uploads", ObjectName: "notes.txt", ContentType: "text/plain", Size: -1,
	}); err != nil {
		t.Fatalf("the upload: %v", err)
	}
	runtime.ReadMemStats(&after)
	if held := after.TotalAlloc - before.TotalAlloc; held > 64<<20 {
		t.Errorf("a five-byte upload took %d MB of memory", held>>20)
	}
	if len(stored) != 1 {
		t.Errorf("stored %v", stored)
	}
}
