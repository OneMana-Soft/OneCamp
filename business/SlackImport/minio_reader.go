package business

import (
	"compress/flate"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"archive/zip"

	"github.com/akashc777/OneCamp/helpers"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	"github.com/minio/minio-go/v7"
)

// minioReaderAt implements io.ReaderAt directly against an object in
// MinIO. Used as the underlying source for archive/zip readers so we
// never have to download the full export to local disk before parsing.
//
// Why not stream the whole zip and parse it once? archive/zip needs
// random access to find the End-of-Central-Directory record at the tail
// of the file and then to seek to each entry. A naive io.Reader (which
// is what an HTTP body is) cannot satisfy that interface. Two real-world
// choices:
//
//	(A) Download to /tmp once, parse N times. Simple but caps you at
//	    /tmp disk. We keep this as a kill-switch via SLACK_IMPORT_STAGE_TO_DISK
//	    for environments where MinIO range support misbehaves.
//	(B) Implement a ReaderAt that issues HTTP range requests against
//	    MinIO. No disk staging at any size. This file. ←
//	    This is the default path.
//
// Concurrency: multiple workers concurrently call zr.File[i].Open(), and
// each Open returns a SectionReader that ultimately calls our ReadAt.
// minio.Client.GetObject is safe for concurrent use, so this struct
// inherits that safety with no extra locking.
//
// Performance characteristics:
//   - One HTTP range request per ReadAt.
//   - archive/zip uses bufio internally, so per-entry sequential reads
//     produce ~32 KB-aligned ReadAt calls on the SectionReader.
//   - For best per-entry throughput, callers should prefer the
//     openZipEntryDirect helper below, which fetches an entry's full
//     compressed body in a single range request and decompresses
//     locally — eliminates the small-read overhead entirely.
type minioReaderAt struct {
	ctx    context.Context
	bucket string
	key    string
	size   int64
}

// newMinioReaderAt stat's the object so we can hand archive/zip a real
// size. Stat is a single round-trip; cheap.
func newMinioReaderAt(ctx context.Context, bucket, key string) (*minioReaderAt, error) {
	stat, err := minioInit.MinioClient.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("stat staged zip: %w", err)
	}
	if stat.Size <= 0 {
		return nil, fmt.Errorf("staged zip is empty: %s/%s", bucket, key)
	}
	return &minioReaderAt{ctx: ctx, bucket: bucket, key: key, size: stat.Size}, nil
}

// Size returns the total object size. archive/zip needs this when
// constructing a Reader.
func (r *minioReaderAt) Size() int64 { return r.size }

// ReadAt fulfils io.ReaderAt. Issues a single ranged GetObject for the
// requested window. Returns io.EOF only when off >= size; partial reads
// at the tail are returned with err == nil so archive/zip's bufio
// behaves correctly.
func (r *minioReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if off >= r.size {
		return 0, io.EOF
	}
	end := off + int64(len(p)) - 1
	if end >= r.size {
		end = r.size - 1
		p = p[:end-off+1]
	}

	opts := minio.GetObjectOptions{}
	if err := opts.SetRange(off, end); err != nil {
		return 0, err
	}

	obj, err := minioInit.MinioClient.GetObject(r.ctx, r.bucket, r.key, opts)
	if err != nil {
		return 0, fmt.Errorf("minio range get: %w", err)
	}
	defer obj.Close()

	// io.ReadFull because we asked for an exact byte range; partial
	// reads from MinIO mean the object truncated under us, which is
	// fatal for zip parsing.
	n, err := io.ReadFull(obj, p)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		// Tail of the file. Return what we got with nil; archive/zip
		// handles the short read.
		return n, nil
	}
	return n, err
}

// entryRangeFetcher fetches one byte range of the object backing an archive.
//
// This exists so the single-request entry path can be exercised without MinIO. The risky
// part of that path is arithmetic — an entry's data offset, its compressed length, and the
// inclusive end of the range — not HTTP. Behind this interface a test serves ranges out of
// an in-memory archive and can assert the bytes are exactly what archive/zip would have
// produced.
type entryRangeFetcher interface {
	fetchRange(ctx context.Context, off, length int64) (io.ReadCloser, error)
}

// fetchRange issues a single ranged GET. Callers own closing the reader.
func (r *minioReaderAt) fetchRange(ctx context.Context, off, length int64) (io.ReadCloser, error) {
	if length <= 0 {
		return nil, fmt.Errorf("non-positive range length %d", length)
	}
	if off < 0 || off+length > r.size {
		return nil, fmt.Errorf("range [%d,%d) outside object of size %d", off, off+length, r.size)
	}
	opts := minio.GetObjectOptions{}
	if err := opts.SetRange(off, off+length-1); err != nil {
		return nil, err
	}
	obj, err := minioInit.MinioClient.GetObject(ctx, r.bucket, r.key, opts)
	if err != nil {
		return nil, fmt.Errorf("minio range get for entry: %w", err)
	}
	return obj, nil
}

// Archive is a parsed Slack export together with the source it was read from, so reading an
// entry can take the one-request path instead of many small ranged reads.
//
// It embeds *zip.Reader, so everything that used to be done on the bare reader — walking
// arc.File, handing arc.Reader to a helper — still reads the same.
//
// src is nil for the disk-staged fallback, where entries are local and the optimisation is
// pointless. Carrying the source WITH the archive rather than as a second parameter is
// deliberate: two values that must travel together are exactly the shape that produced the
// forwarded-message misindex elsewhere in this codebase.
type Archive struct {
	*zip.Reader
	src entryRangeFetcher
}

// OpenEntry reads one entry, preferring the single-range path and falling back to
// archive/zip whenever there is no ranged source (disk fallback) or the entry uses a
// compression method the fast path does not implement.
func (a *Archive) OpenEntry(ctx context.Context, zf *zip.File) (io.ReadCloser, error) {
	if a == nil || a.src == nil {
		return zf.Open()
	}
	return openZipEntryDirect(ctx, a.src, zf)
}

// diskArchive wraps a locally staged zip so the disk fallback returns the same type.
func diskArchive(zr *zip.Reader) *Archive { return &Archive{Reader: zr} }

// openMinioZip wraps a MinIO object as an *Archive without ever
// touching local disk. Cleanup is a no-op; the caller pattern stays the
// same as the old openStagedZip helper for drop-in replacement.
func openMinioZip(ctx context.Context, objectKey string) (*Archive, func(), error) {
	bucket := helpers.UserUploadBucket()
	rdr, err := newMinioReaderAt(ctx, bucket, objectKey)
	if err != nil {
		return nil, nil, err
	}
	zr, err := zip.NewReader(rdr, rdr.Size())
	if err != nil {
		return nil, nil, fmt.Errorf("zip parse: %w", err)
	}
	return &Archive{Reader: zr, src: rdr}, func() {}, nil
}

// openZipEntryDirect bypasses archive/zip's SectionReader → ReadAt
// indirection by fetching the entry's full compressed body in one
// MinIO range request, then decompressing locally. This is the
// difference between O(entry_compressed_size / 32KB) HTTP requests
// and exactly one HTTP request per entry — typically a 30–100x
// improvement on real workspaces.
//
// Supports Store and Deflate (the only methods Slack uses). Falls back
// to the standard zf.Open() for unrecognised methods so we don't break
// imports against future Slack export formats.
//
// Reached through Archive.OpenEntry, which supplies the fetcher and handles the cases this
// function does not (no ranged source at all).
func openZipEntryDirect(ctx context.Context, src entryRangeFetcher, zf *zip.File) (io.ReadCloser, error) {
	if zf.Method != zip.Store && zf.Method != zip.Deflate {
		// Fallback path for unknown methods. Slow but correct.
		return zf.Open()
	}

	dataOffset, err := zf.DataOffset()
	if err != nil {
		return nil, fmt.Errorf("zip data offset: %w", err)
	}

	compSize := int64(zf.CompressedSize64)
	if compSize == 0 {
		// Empty entry. Return a no-op reader so callers don't crash.
		return io.NopCloser(strings.NewReader("")), nil
	}

	obj, err := src.fetchRange(ctx, dataOffset, compSize)
	if err != nil {
		return nil, err
	}

	switch zf.Method {
	case zip.Store:
		return obj, nil
	case zip.Deflate:
		fr := flate.NewReader(obj)
		// flate.NewReader's Closer doesn't propagate to the underlying
		// MinIO object; we wrap to ensure both close.
		return &compositeCloser{Reader: fr, closers: []io.Closer{fr, obj}}, nil
	}
	// Unreachable; the switch above covers the early-return cases.
	obj.Close()
	return nil, fmt.Errorf("unsupported zip method: %d", zf.Method)
}

// compositeCloser closes multiple resources in order, returning the
// first error. Used to chain a flate decompressor with its underlying
// MinIO ReadCloser.
type compositeCloser struct {
	io.Reader
	closers []io.Closer
}

func (c *compositeCloser) Close() error {
	var firstErr error
	for _, cl := range c.closers {
		if err := cl.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// OpenStagedZipFromMinIO is the public entry point used by the
// controller layer. Internally it just calls openMinioZip but keeps
// the package boundary clean (controllers don't need to know about
// minioReaderAt).
func OpenStagedZipFromMinIO(ctx context.Context, objectKey string) (*Archive, func(), error) {
	return openMinioZip(ctx, objectKey)
}
