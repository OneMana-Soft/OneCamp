package helpers

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The production Go the code-quality ratchets read (slopMarkersGuard_test.go,
// copiedCodeGuard_test.go): what ships in the server, without tests, testdata
// or the commands (cmd, other-services, scripts), whose prints are their output.
var productionGoRoots = []string{
	"../business", "../controllers", "../domain", "../helpers", "../middleware",
	"../models", "../services", "../adapter", "../router", "../initializers",
}

// productionGoFiles walks productionGoRoots for .go files that aren't tests,
// testdata or generated, failing when a root is missing: a renamed root would
// otherwise turn a ratchet into a check that scans nothing.
func productionGoFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, root := range productionGoRoots {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("scan root %s missing, so this check would silently pass: %v", root, err)
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "testdata" || name == "vendor" || name == "node_modules" {
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") && !strings.HasSuffix(path, ".pb.go") {
				out = append(out, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	sort.Strings(out)
	return out
}

// checkRatchet holds counts found now (by key: "path" or "path marker") to a
// baseline of "key count" lines. It fails on a key the baseline doesn't list,
// on a count above its line, and on a count below its line or a line whose key
// is gone: progress is written into the baseline, so it shows in the diff and
// can't creep back. With writeEnv set in the environment it writes the
// baseline instead (header first), for the change that sets it on purpose.
func checkRatchet(t *testing.T, found map[string]int, baselinePath, header, writeEnv, advice string) {
	t.Helper()
	keys := make([]string, 0, len(found))
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if os.Getenv(writeEnv) != "" {
		var b strings.Builder
		b.WriteString(header)
		for _, k := range keys {
			fmt.Fprintf(&b, "%s %d\n", k, found[k])
		}
		if err := os.WriteFile(baselinePath, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d lines to %s", len(keys), baselinePath)
		return
	}
	baseline, err := readRatchetBaseline(baselinePath)
	if err != nil {
		t.Fatalf("reading %s: %v", baselinePath, err)
	}
	var appeared, grown, shrunk []string
	for _, k := range keys {
		allowed, listed := baseline[k]
		switch {
		case !listed:
			appeared = append(appeared, fmt.Sprintf("%s (%d)", k, found[k]))
		case found[k] > allowed:
			grown = append(grown, fmt.Sprintf("%s: %d, baseline %d", k, found[k], allowed))
		case found[k] < allowed:
			shrunk = append(shrunk, fmt.Sprintf("%s: %d, baseline %d", k, found[k], allowed))
		}
	}
	for k := range baseline {
		if _, still := found[k]; !still {
			shrunk = append(shrunk, fmt.Sprintf("%s: gone, baseline %d", k, baseline[k]))
		}
	}
	sort.Strings(shrunk)
	if len(appeared)+len(grown) > 0 {
		t.Errorf("new (%d):\n  %s\n\n%s", len(appeared)+len(grown), strings.Join(append(appeared, grown...), "\n  "), advice)
	}
	if len(shrunk) > 0 {
		t.Errorf("progress: lower these in %s (or run with %s=1) so they can't creep back:\n  %s",
			baselinePath, writeEnv, strings.Join(shrunk, "\n  "))
	}
}

// readRatchetBaseline parses "key count" lines (a key may hold spaces; the
// count is the last field), ignoring comments and blank lines.
func readRatchetBaseline(path string) (map[string]int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cut := strings.LastIndexByte(line, ' ')
		if cut < 0 {
			return nil, fmt.Errorf("line %d: want \"key count\", got %q", i+1, line)
		}
		n, convErr := strconv.Atoi(line[cut+1:])
		if convErr != nil {
			return nil, fmt.Errorf("line %d: %q is not a count: %w", i+1, line[cut+1:], convErr)
		}
		out[line[:cut]] = n
	}
	return out, nil
}
