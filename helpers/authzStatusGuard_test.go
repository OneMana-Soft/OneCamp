package helpers

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 401 means "I do not know who you are". 403 means "I know, and no".
//
// The frontend's axios interceptor believes 401 literally: it fires a token
// refresh and re-issues the request, and if that refresh fails in the window it
// logs the user out. So answering a MEMBERSHIP question with 401 spends a
// refresh and a duplicate request on a permissions answer that was never going
// to change, and a routine validation error did the same.
//
// This pins the direction of travel: no new membership or ownership check may
// answer 401. It reads the condition guarding each 401 rather than the message,
// because the messages were already inconsistent ("Not Authorised", "Not
// authorised", "Unauthorized") while the conditions were not.
func TestAuthorisationChecksDoNotAnswer401(t *testing.T) {
	// Unambiguous authorisation predicates. Deliberately narrow: a token or
	// signature check on a public endpoint is a real 401 and must not match.
	authz := regexp.MustCompile(`\b(` +
		`IsMember|IsProjectMember|IsProjectAdmin|ParticipantIsMember|` +
		`HasEditAccess|isParticipant|isChannelMember|isComentOwner` +
		`)\b|(CreatedBy|AddedBy|CommentBy|PostBy)\.(Uuid|Uid)\s*!=`)

	var offenders []string

	err := filepath.Walk("../controllers", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		lines := strings.Split(string(src), "\n")
		for i, line := range lines {
			if !strings.Contains(line, "http.StatusUnauthorized") {
				continue
			}
			// Commented-out code is not a live answer.
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			cond := ""
			for j := i - 1; j >= 0 && j > i-8; j-- {
				if strings.HasPrefix(strings.TrimSpace(lines[j]), "if ") {
					cond = strings.TrimSpace(lines[j])
					break
				}
			}
			if authz.MatchString(cond) {
				offenders = append(offenders, filepath.ToSlash(path)+":"+itoa(i+1)+"  "+truncate(cond, 60))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk failed: %v", err)
	}

	if len(offenders) > 0 {
		t.Errorf("membership/ownership checks answering 401 (use http.StatusForbidden; "+
			"401 makes the frontend refresh the token and can log the user out):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
