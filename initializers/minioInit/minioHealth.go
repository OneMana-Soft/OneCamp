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

	"github.com/akashc777/OneCamp/helpers"
)

func init() {
	helpers.RegisterSystemCheck(helpers.SystemCheck{
		Name: "storage",
		Kind: helpers.CheckKindDependency,
		Describe: "MinIO is reachable and the bucket uploads go to exists. It does not prove a write would " +
			"succeed, since proving that would mean putting an object in your bucket.",
		Probe: func(ctx context.Context) error {
			if MinioClient == nil {
				return fmt.Errorf("no MinIO client: object storage was never initialised, so no file can be uploaded or served")
			}

			bucket := helpers.UserUploadBucket()
			exists, err := MinioClient.BucketExists(ctx, bucket)
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
		},
	})
}
