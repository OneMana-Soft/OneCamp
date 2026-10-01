package helpers

import "os"

// UserUploadBucket is the one place that decides where user files live.
//
// WHY THIS IS A FUNCTION. Twenty-four call sites read USER_UPLOAD_BUCKET_NAME by
// hand and they did not agree. Eighteen fell back to "onecamp-user-upload" when
// it was unset; six used the empty string. So on an install missing the variable,
// the upload path and the download path would look in two different places, and
// the six would hand MinIO a bucket name of "" — which fails, with an error that
// says nothing about configuration.
//
// The fallback is kept byte-identical to what those eighteen already did, so this
// changes no behaviour on any install that sets the variable, which every shipped
// template does. What it changes is that the six now agree with the eighteen, and
// that a wrong answer here is DETECTABLE: the storage system check reports an
// unset variable and a missing bucket by name, instead of every upload failing
// with a MinIO error.
//
// The fallback deliberately matches none of the shipped templates
// (onecamp-uploads in .env.prod, test in .env.beta and .env.local). That is not a
// mistake to be tidied away by "fixing" it to one of them: it means a bucket
// reached through the fallback will not exist, the check will say so, and the
// operator learns their variable is missing rather than silently writing into a
// bucket that belongs to another environment.
const defaultUserUploadBucket = "onecamp-user-upload"

func UserUploadBucket() string {
	if b := os.Getenv("USER_UPLOAD_BUCKET_NAME"); b != "" {
		return b
	}
	return defaultUserUploadBucket
}

// UserUploadBucketIsConfigured reports whether the operator actually set the
// variable, as opposed to falling through to a default that exists nowhere.
// Separate from UserUploadBucket because callers want the name and only the
// health check wants to know where it came from.
func UserUploadBucketIsConfigured() bool {
	return os.Getenv("USER_UPLOAD_BUCKET_NAME") != ""
}
