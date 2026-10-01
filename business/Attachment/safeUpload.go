// Package business — generic safe-upload helper shared by every code
// path that writes user-supplied bytes into MinIO.
//
// Why this lives here:
//   - The native chat-upload path (controllers/User), the Slack
//     import file_worker, and the generic-import attachment_worker
//     all need the same three things: stream into MinIO, AV-scan
//     concurrently, force a safe Content-Type / Content-Disposition.
//   - Putting it in any one of those packages would force an import
//     cycle through the others. The Attachment business package is
//     already a leaf consumed by all uploaders.
package business

import (
	"context"
	"fmt"
	"io"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/avscan"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	"github.com/minio/minio-go/v7"
)

// SafeUploadOptions controls how SafeUploadToMinio writes an object.
type SafeUploadOptions struct {
	// Bucket is the destination MinIO bucket.
	Bucket string

	// ObjectName is the MinIO key to write.
	ObjectName string

	// ContentType is the value to send on PutObject. Callers MUST
	// have already passed it through uploadsafe.SafeContentType so a
	// dangerous extension won't surface as text/html on the
	// presigned GET.
	ContentType string

	// ContentDisposition, when non-empty, is set on the MinIO object
	// metadata so every presigned download surfaces it. Default
	// callers should pass uploadsafe.BuildDisposition(name).
	ContentDisposition string

	// Size is the declared body size. -1 enables MinIO multipart
	// upload (used when streaming with unknown size).
	Size int64
}

// SafeUploadToMinio streams body into MinIO and concurrently scans it
// with the configured AV scanner (if any). Returns the MinIO upload
// info, the AV verdict, and any error encountered.
//
// Behaviour matrix:
//
//	scanner=nil               → single-pass write, Verdict=Unknown
//	scanner ok, clean         → write completes, Verdict=Clean
//	scanner ok, infected      → returned with Verdict=Infected; caller
//	                            is responsible for deleting the
//	                            written object (we don't auto-delete
//	                            because the caller may want to log /
//	                            quarantine first).
//	scanner error, fail-open  → write completes, Verdict=Unknown
//	scanner error, fail-close → write rolled back, error returned
//
// Concurrency:
//   - One goroutine reads from the original body via io.TeeReader
//     into an io.Pipe; PutObject reads from the tee, the scanner
//     reads from the pipe.
//   - On error in either branch we close the pipe so the other
//     branch returns immediately.
func SafeUploadToMinio(ctx context.Context, body io.Reader, opts SafeUploadOptions) (
	minio.UploadInfo, avscan.Result, error) {

	scanner := avscan.Default()
	putOpts := minio.PutObjectOptions{
		ContentType:        opts.ContentType,
		ContentDisposition: opts.ContentDisposition,
	}

	if scanner == nil {
		// No scanner configured: single read, single write.
		ui, err := minioInit.MinioClient.PutObject(ctx, opts.Bucket, opts.ObjectName,
			body, opts.Size, putOpts)
		return ui, avscan.Result{Verdict: avscan.VerdictUnknown}, err
	}

	pr, pw := io.Pipe()
	tee := io.TeeReader(body, pw)

	var (
		ui      minio.UploadInfo
		uiErr   error
		scanRes avscan.Result
		scanErr error
		done    = make(chan struct{}, 2)
	)

	// AV consumer.
	go func() {
		defer func() { done <- struct{}{} }()
		scanRes, scanErr = scanner.Scan(ctx, pr)
		// Drain to make sure the producer can exit even if the
		// scanner reads less than the full body (clamd may abort
		// early once a signature matches).
		_, _ = io.Copy(io.Discard, pr)
		_ = pr.Close()
	}()

	// MinIO consumer.
	go func() {
		defer func() { done <- struct{}{} }()
		ui, uiErr = minioInit.MinioClient.PutObject(ctx, opts.Bucket, opts.ObjectName,
			tee, opts.Size, putOpts)
		if uiErr != nil {
			_ = pw.CloseWithError(uiErr)
		} else {
			_ = pw.Close()
		}
	}()

	<-done
	<-done

	if uiErr != nil {
		return ui, scanRes, uiErr
	}
	if scanErr != nil {
		if avscan.FailOpen() {
			helpers.LogWarnWithContext(ctx,
				"AV scan failed (fail-open: allowing): bucket=%s key=%s err=%+v",
				opts.Bucket, opts.ObjectName, scanErr)
			return ui, avscan.Result{Verdict: avscan.VerdictUnknown}, nil
		}
		// Fail-closed: roll back and surface error.
		_ = minioInit.MinioClient.RemoveObject(context.Background(), opts.Bucket, opts.ObjectName,
			minio.RemoveObjectOptions{})
		return ui, scanRes, fmt.Errorf("av scan error (fail-closed): %w", scanErr)
	}
	return ui, scanRes, nil
}
