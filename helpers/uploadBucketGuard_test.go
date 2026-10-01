package helpers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The upload bucket is resolved in ONE place.
//
// It was resolved in twenty-four, and they disagreed: eighteen fell back to
// "onecamp-user-upload" when USER_UPLOAD_BUCKET_NAME was unset and six used the
// empty string, so an install missing the variable would write with one name and
// read with another, and the six would hand MinIO "" and fail with an error that
// says nothing about configuration.
//
// A tree walk rather than a list of files. Twice this session a guard written as
// a hand-maintained file list passed while naming only some of the offenders,
// which is the failure mode that matters here: a new call site is exactly the
// thing that would not be on the list.
func TestUploadBucketIsResolvedInOnePlace(t *testing.T) {
	root := ".."
	// This file and its guard are the one place, so they are the only exemption.
	allowed := map[string]bool{
		filepath.Join(root, "helpers", "uploadBucket.go"):           true,
		filepath.Join(root, "helpers", "uploadBucketGuard_test.go"): true,
	}

	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case "node_modules", ".git", "data", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || allowed[path] {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		text := string(src)
		if strings.Contains(text, `os.Getenv("USER_UPLOAD_BUCKET_NAME")`) {
			offenders = append(offenders, path+`: reads USER_UPLOAD_BUCKET_NAME directly`)
		}
		if strings.Contains(text, `"onecamp-user-upload"`) {
			offenders = append(offenders, path+`: repeats the default bucket name`)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}

	if len(offenders) > 0 {
		t.Errorf("the upload bucket must come from helpers.UserUploadBucket(), not from here (%d):\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

func TestUserUploadBucketPrefersTheConfiguredValue(t *testing.T) {
	t.Setenv("USER_UPLOAD_BUCKET_NAME", "my-bucket")
	if got := UserUploadBucket(); got != "my-bucket" {
		t.Errorf("configured bucket: got %q, want %q", got, "my-bucket")
	}
	if !UserUploadBucketIsConfigured() {
		t.Error("a set variable must report as configured")
	}
}

func TestAnUnsetBucketIsReportedAsUnconfigured(t *testing.T) {
	t.Setenv("USER_UPLOAD_BUCKET_NAME", "")
	// The name still resolves, so no caller has to handle an empty bucket. What
	// changes is that the health check can tell an operator WHY their uploads
	// fail, instead of leaving them a MinIO error about a bucket they never chose.
	if got := UserUploadBucket(); got == "" {
		t.Error("an unset variable must still yield a usable name, or callers pass \"\" to MinIO")
	}
	if UserUploadBucketIsConfigured() {
		t.Error("an unset variable must not report as configured")
	}
}
