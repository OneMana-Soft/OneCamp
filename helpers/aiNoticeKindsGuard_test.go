package helpers

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A notice kind is how the Inbox titles and opens a notice about one kind of
// thing (business/Follow RegisterNoticeKind), registered from its package's
// init. A kind registered in an AI package isn't registered at all in the
// edition without AI, which doesn't have that package: its notices list
// there untitled and open nothing. The channel and conversation kinds were
// registered only by business/AIAgent, so a mention told in a channel or a
// DM (message.mentioned, NOTIF-H3) would have reached nobody's Inbox whole
// there. So every kind is registered outside the AI packages (isAIPackage).
// An AI-only file itself (aiOnlyFileName): the edition without AI has no AI
// package to look in.

func TestNoticeKindsAreRegisteredInBothEditions(t *testing.T) {
	var inAI []string
	err := filepath.Walk("../business", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		pkg := filepath.ToSlash(filepath.Dir(strings.TrimPrefix(filepath.ToSlash(path), "../")))
		if !isAIPackage(pkg) {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(src), "\n") {
			code := strings.TrimSpace(line)
			if strings.HasPrefix(code, "//") || !strings.Contains(code, "RegisterNoticeKind(") {
				continue
			}
			inAI = append(inAI, filepath.ToSlash(path)+":"+strconv.Itoa(i+1)+": "+code)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range inAI {
		t.Errorf("%s: a notice kind registered in an AI package is missing from the edition without AI; register it in the package of the thing it is about", at)
	}
}
