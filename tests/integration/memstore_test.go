//go:build integration
// +build integration

package integration_test

// memStore stands in for MinIO: the objects a test starts with, served by the
// end of their keys, and whatever the code under test writes, by its path: a
// plain PUT, or a multipart upload (an object written as it streams, its size
// not known ahead: the Slack import's API read, an imported file). Bodies
// minio-go sends in aws-chunked framing over plain http are unframed.

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type memStore struct {
	mu      sync.Mutex
	seeded  map[string][]byte         // by the end of the key
	written map[string][]byte         // by path, /bucket/key
	uploads map[string]map[int][]byte // multipart uploads under way, by id
	next    int
}

func newMemStore(seeded map[string][]byte) *memStore {
	return &memStore{seeded: seeded, written: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
}

// object is what the store holds at p.
func (s *memStore) object(p string) ([]byte, bool) {
	if body, ok := s.written[p]; ok {
		return body, true
	}
	for key, body := range s.seeded {
		if strings.HasSuffix(p, "/"+key) {
			return body, true
		}
	}
	return nil, false
}

// Written is the object written at the end of key, if one was.
func (s *memStore) Written(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for p, body := range s.written {
		if strings.HasSuffix(p, "/"+key) {
			return body, true
		}
	}
	return nil, false
}

// Keys is every object written, by path.
func (s *memStore) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for p := range s.written {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (s *memStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := r.URL.Query()
	p := r.URL.Path
	etag := func(b []byte) string { return fmt.Sprintf(`"%x"`, md5.Sum(b)) }
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		s.next++
		id := fmt.Sprintf("upload-%d", s.next)
		s.uploads[id] = map[int][]byte{}
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult><Bucket>b</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, path.Base(p), id)
	case r.Method == http.MethodPut && q.Get("uploadId") != "":
		parts := s.uploads[q.Get("uploadId")]
		n, err := strconv.Atoi(q.Get("partNumber"))
		body, rerr := unframed(r)
		if parts == nil || err != nil || rerr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		parts[n] = body
		w.Header().Set("ETag", etag(body))
	case r.Method == http.MethodPost && q.Get("uploadId") != "":
		parts := s.uploads[q.Get("uploadId")]
		if parts == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		numbers := make([]int, 0, len(parts))
		for n := range parts {
			numbers = append(numbers, n)
		}
		sort.Ints(numbers)
		var all []byte
		for _, n := range numbers {
			all = append(all, parts[n]...)
		}
		delete(s.uploads, q.Get("uploadId"))
		s.written[p] = all
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><CompleteMultipartUploadResult><Location>%s</Location><Bucket>b</Bucket><Key>%s</Key><ETag>%s</ETag></CompleteMultipartUploadResult>`, p, path.Base(p), etag(all))
	case r.Method == http.MethodDelete && q.Get("uploadId") != "":
		delete(s.uploads, q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		body, err := unframed(r)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.written[p] = body
		w.Header().Set("ETag", etag(body))
	case r.Method == http.MethodDelete:
		// An object the test started with stays: tests read it again.
		delete(s.written, p)
		w.WriteHeader(http.StatusNoContent)
	default:
		body, ok := s.object(p)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", `"0123456789abcdef0123456789abcdef"`)
		http.ServeContent(w, r, path.Base(p), time.Now(), bytes.NewReader(body))
	}
}

// unframed is a request's body, without the aws-chunked framing minio-go
// streams a signed body in over plain http ("<hex size>;chunk-signature=…",
// the bytes, a CRLF; a zero-size chunk, then any trailers).
func unframed(r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") && !strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") {
		return raw, nil
	}
	in := bufio.NewReader(bytes.NewReader(raw))
	var out []byte
	for {
		line, err := in.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("a chunk's header: %w", err)
		}
		size, err := strconv.ParseInt(strings.TrimSpace(strings.SplitN(line, ";", 2)[0]), 16, 64)
		if err != nil {
			return nil, fmt.Errorf("a chunk's size %q: %w", line, err)
		}
		if size == 0 {
			break
		}
		chunk := make([]byte, size)
		if _, err := io.ReadFull(in, chunk); err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		if _, err := in.Discard(2); err != nil {
			return nil, err
		}
	}
	if want := r.Header.Get("X-Amz-Decoded-Content-Length"); want != "" && want != strconv.Itoa(len(out)) {
		return nil, fmt.Errorf("unframed %d bytes, the request says %s", len(out), want)
	}
	return out, nil
}
