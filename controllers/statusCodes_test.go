package controllers

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 401 means "sign in again". The web app answers one by refreshing the session
// and shows no error, so a 401 for "you may not do this" made a refused action
// look like nothing happened (35 handlers did, until October 2026). A
// permission check answers 403, a bad request 400.
func TestPermissionChecksAnswer403Not401(t *testing.T) {
	permission := regexp.MustCompile(`IsAdmin|canManage|canView|IsProjectMember|SrcKey|IsValidName|IsModerator`)
	checked := 0
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, _ := os.ReadFile(path)
		lines := strings.Split(string(src), "\n")
		for i, line := range lines {
			if !strings.Contains(line, "http.StatusUnauthorized") || strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			checked++
			for j := i - 1; j >= 0 && j >= i-4; j-- {
				l := strings.TrimSpace(lines[j])
				if strings.HasPrefix(l, "if ") || strings.HasPrefix(l, "} else if") {
					if permission.MatchString(l) {
						t.Errorf("%s:%d answers 401 to a permission check (%s); use 403", path, i+1, l)
					}
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 10 {
		t.Fatalf("found only %d 401s; the scan is looking in the wrong place", checked)
	}
}

// The auth middleware stores the signed-in person as a UserInfo value. A
// handler that reads it as *UserInfo never finds it, and answers 401 to
// everyone: creating webhooks, archive runs and deleting recordings all did,
// until October 2026. Use userModel.FromContext, which takes either.
func TestHandlersReadTheSignedInPersonAsStored(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, _ := os.ReadFile(path)
		if regexp.MustCompile(`UserInfoContextKey\)\.\(\*`).Match(src) {
			t.Errorf("%s reads the signed-in person as a pointer, which never matches; use userModel.FromContext", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
