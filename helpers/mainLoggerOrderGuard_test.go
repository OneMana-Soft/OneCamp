package helpers

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// main() must not touch MessageLogs before InitLogger has run.
//
// THE CRASH LOOP THIS EXISTS FOR. MessageLogs is nil until loggerInit.InitLogger
// runs, which main does after loading the environment and a couple of other
// things. A log line added above that point dereferences nil, panics before
// any handler exists to recover it, and the container restarts forever with a
// stack trace that names the log call. It shipped once, to the demo, for the
// time it took to read the stack. cmd/server cannot carry this test itself
// because its init refuses to run without a .env, so it lives here and reads
// the source.
//
// Log*WithContext is the nil-safe family and is fine anywhere; this checks
// only the direct MessageLogs field access.
func TestMainDoesNotLogBeforeTheLoggerExists(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "cmd", "server", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	mainStart := strings.Index(s, "\nfunc main() {")
	if mainStart < 0 {
		t.Fatal("no main()")
	}
	body := s[mainStart:]
	initAt := strings.Index(body, "loggerInit.InitLogger()")
	if initAt < 0 {
		t.Fatal("main() no longer calls loggerInit.InitLogger(); this guard needs updating")
	}
	// Comments are not code; a commented-out recover block mentions the field.
	before := regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(body[:initAt], "")
	if i := strings.Index(before, "helpers.MessageLogs"); i >= 0 {
		line := 1 + strings.Count(s[:mainStart], "\n") + strings.Count(before[:i], "\n")
		t.Fatalf("cmd/server/main.go:%d uses helpers.MessageLogs before InitLogger runs; "+
			"it is nil there and the server crash-loops at boot. Use helpers.LogInfoWithContext "+
			"or LogErrorWithContext, which are nil-safe.", line)
	}
}
