package mdpathology

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RealDoc is this repository's own Markdown end to end, read again from the
// start until it is at least size bytes: its docs first, then its READMEs,
// licences and the SDKs' READMEs. A real doc of headings, lists, tables,
// code blocks and links, as long as a large project's README. The repository
// is found from the test's working directory (its go.mod), so any package's
// test may ask, and the public export (which keeps docs/ and moves
// public-repo/'s files to its root) reads the same kind of text.
func RealDoc(size int) (string, error) {
	root, err := moduleRoot()
	if err != nil {
		return "", err
	}
	var files []string
	for _, pattern := range []string{"docs/*.md", "public-repo/*.md", "*.md", "sdk/*/README.md"} {
		m, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			return "", err
		}
		sort.Strings(m)
		files = append(files, m...)
	}
	var b strings.Builder
	for b.Len() < size {
		read := 0
		for _, f := range files {
			body, err := os.ReadFile(f)
			if err != nil {
				return "", err
			}
			read += len(body)
			b.Write(body)
			b.WriteString("\n\n")
		}
		if read == 0 {
			return "", fmt.Errorf("mdpathology: no Markdown found under %s", root)
		}
	}
	return b.String(), nil
}

// moduleRoot is the nearest directory at or above the working directory
// holding a go.mod.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("mdpathology: no go.mod at or above the working directory")
		}
		dir = parent
	}
}
