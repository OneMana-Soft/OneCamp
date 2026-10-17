package minioInit

// Can this install actually store a file?
//
// THE DAY-ONE FAILURE. Connecting to MinIO and having somewhere to put things
// are different questions, and only the first is asked at boot. "Create MinIO
// Bucket" is its own step in the install guide, so it is a step that can be
// skipped, and when it is, every upload fails with an S3 error naming a bucket
// the operator never chose. Nothing connects that to the step they missed.
//
// The variable matters as much as the bucket. USER_UPLOAD_BUCKET_NAME is
// "onecamp-uploads" in the shipped production template and "test" in the beta
// one, while the built-in fallback is neither; an install that lost the variable
// therefore reads and writes a bucket that exists in no environment. Saying so
// plainly is the whole value here.
//
// WHAT IT DOES NOT PROVE. That a write would succeed. Credentials can be
// read-only and a policy can forbid PutObject, and finding that out would mean
// writing an object into somebody's bucket, which this must never do.

import (
	"context"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// storageProbeWaits are the pauses between the storage check's looks.
//
// RIGHT AFTER A START, ONE LOOK CAN BE WRONG. The API reaches MinIO by its
// public name, through Traefik, and for a moment after the stack starts that
// name may not route yet. On the demo's deploy of 17 Oct 2026 the first
// readiness check read "FAIL storage" and the three after it read ok; a
// Traefik 404 even reads as "no such bucket" to the S3 client. So a look that
// fails is tried twice more, after half a second and then a second, inside the
// check's own deadline, and only the last answer is reported. Storage that is
// really down, or a bucket that is really missing, still fails, 1.5 s later.
var storageProbeWaits = []time.Duration{500 * time.Millisecond, time.Second}

// lookBucket asks MinIO whether the bucket exists. Tests stand in for it.
var lookBucket = func(ctx context.Context, bucket string) (bool, error) {
	return MinioClient.BucketExists(ctx, bucket)
}

func init() {
	helpers.RegisterSystemCheck(helpers.SystemCheck{
		Name: "storage",
		Kind: helpers.CheckKindDependency,
		Describe: "MinIO is reachable and the bucket uploads go to exists. It does not prove a write would " +
			"succeed, since proving that would mean putting an object in your bucket.",
		Probe: probeStorage,
	})
}

// probeStorage is the "storage" check: MinIO answers, and the uploads bucket is there.
func probeStorage(ctx context.Context) error {
	if MinioClient == nil {
		return fmt.Errorf("no MinIO client: object storage was never initialised, so no file can be uploaded or served")
	}

	bucket := helpers.UserUploadBucket()
	exists, err := lookSettled(ctx, bucket)
	if err != nil {
		return fmt.Errorf("could not reach MinIO to look for bucket %q: %w", bucket, err)
	}
	if !exists {
		if !helpers.UserUploadBucketIsConfigured() {
			return fmt.Errorf("bucket %q does not exist, and USER_UPLOAD_BUCKET_NAME is not set so this "+
				"name is a built-in default that matches no shipped configuration; set the variable to "+
				"your bucket, or create this one", bucket)
		}
		return fmt.Errorf("bucket %q does not exist; uploads will fail until it is created "+
			"(USER_UPLOAD_BUCKET_NAME points at it)", bucket)
	}
	return nil
}

// lookSettled looks for the bucket, and again after each of storageProbeWaits
// while the answer is a failure and the context has time left.
func lookSettled(ctx context.Context, bucket string) (bool, error) {
	exists, err := lookBucket(ctx, bucket)
	for _, wait := range storageProbeWaits {
		if err == nil && exists {
			break
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return exists, err
		case <-timer.C:
		}
		exists, err = lookBucket(ctx, bucket)
	}
	return exists, err
}
