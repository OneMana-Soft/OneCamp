package controllers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Text a model really writes: a Windows path, a regex, a tab, quotes, a
// literal backslash-n in code, a control character, and text beyond ASCII.
// Each comes back from its frame exactly as it went in.
func TestSSEFrameCarriesAnyText(t *testing.T) {
	for _, text := range []string{
		`C:\path\new`,
		"col1\tcol2",
		`she said "hi"`,
		`fmt.Println("a\nb")`,
		"line one\nline two\r\n",
		"bell\a and nul\x00",
		"naïve café — 日本語 😀",
		"\u2028\u2029",
		`\`,
	} {
		frame := sseFrame(map[string]string{"content": text})
		if !strings.HasPrefix(frame, "data: ") || !strings.HasSuffix(frame, "\n\n") {
			t.Fatalf("%q: not one SSE data frame: %q", text, frame)
		}
		body := strings.TrimSuffix(strings.TrimPrefix(frame, "data: "), "\n\n")
		if strings.ContainsAny(body, "\r\n") {
			t.Errorf("%q: the frame breaks across lines: %q", text, frame)
		}
		var got struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Errorf("%q: the frame isn't JSON (%v): %s", text, err, body)
			continue
		}
		if got.Content != text {
			t.Errorf("round trip: got %q, want %q", got.Content, text)
		}
	}
}

// No stream builds a frame's JSON by hand. A format verb inside a literal
// that starts a data frame with an object ("data: {\"content\": \"%s\"}") is
// how the doc and Ask AI streams lost every chunk with a backslash or a tab
// in it. A whole frame of marshalled JSON ("data: %s") is fine.
func TestNoStreamBuildsAFrameByHand(t *testing.T) {
	handBuilt := regexp.MustCompile(`"data: \{(\\.|[^"\\\n])*%`)
	root := ".."
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(src), "\n") {
			if handBuilt.MatchString(line) {
				t.Errorf("%s:%d builds an SSE frame by hand; use sseFrame (or json.Marshal the whole payload): %s", path, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
