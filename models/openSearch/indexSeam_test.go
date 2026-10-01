package models

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// indexSeam_test.go — pins the SEAM, not just the sanitiser.
//
// A guard that only exists in a helper is one forgotten call site away from being
// useless, and that is exactly how the original failure happened: the sanitising
// tools existed in the codebase and the write path did not use them. This test
// reads the model sources and fails if any of them builds a request body from raw
// marshalled JSON instead of going through IndexReader/IndexBody.
//
// Comments are stripped before matching, because prose describing the old pattern
// (there is plenty, explaining why it was wrong) must not be mistaken for the
// pattern itself.

var (
	lineCommentRe  = regexp.MustCompile(`//[^\n]*`)
	blockCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
	// rawBodyRe matches the pattern that bypassed the guard: handing marshalled
	// bytes straight to the cluster as a reader.
	rawBodyRe = regexp.MustCompile(`strings\.NewReader\(\s*string\(\s*\w*[jJ]son\w*\s*\)\s*\)`)
	// rawBulkBodyRe matches the same bypass in its BULK form. The bulk body is
	// assembled as NDJSON in the domain layer and arrives here as a plain string, so
	// it does not look like marshalled JSON and rawBodyRe cannot see it — yet it
	// carries most of the write volume (every post/comment created with attachments,
	// every cascading rename and delete). It must go through BulkReader.
	rawBulkBodyRe = regexp.MustCompile(`strings\.NewReader\(\s*bulk\w*\s*\)`)
)

func stripComments(src string) string {
	return lineCommentRe.ReplaceAllString(blockCommentRe.ReplaceAllString(src, " "), " ")
}

// modelSourceFiles walks the sibling model packages (../openSearch/<Entity>/*.go).
func modelSourceFiles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	root := "."
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		out[p] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 5 {
		t.Fatalf("expected to find the model sources; found %d files", len(out))
	}
	return out
}

// TestNoModelBypassesTheIndexSeam is the ratchet: every document sent to OpenSearch
// must be sanitised on the way out.
func TestNoModelBypassesTheIndexSeam(t *testing.T) {
	var offenders []string
	for path, src := range modelSourceFiles(t) {
		clean := stripComments(src)
		if rawBodyRe.MatchString(clean) {
			offenders = append(offenders, path+" (single document — use IndexReader/IndexBody)")
		}
		if rawBulkBodyRe.MatchString(clean) {
			offenders = append(offenders, path+" (bulk body — use BulkReader)")
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("these files send raw JSON to OpenSearch instead of going through the "+
			"seam, which is how an unbounded document reached the cluster and OOM-killed "+
			"the node:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// TestNoLayerOutsideModelsBypassesTheSeam widens the ratchet beyond this package.
//
// It exists because the original version did not, and missed a real one: the AI
// embeddings writer lives in services/AI and wrote to OpenSearch with
// strings.NewReader(string(jsonData)), so raw document bodies (including an inlined
// image) reached the ai_embeddings index while every test here passed. A guard that
// only inspects the directory it lives in gives false confidence about the codebase.
func TestNoLayerOutsideModelsBypassesTheSeam(t *testing.T) {
	// Layers that may legitimately talk to OpenSearch. data/ is excluded because it
	// holds container volumes, not source (and is not readable).
	layers := []string{
		"../../services", "../../business", "../../domain",
		"../../controllers", "../../adapter", "../../initializers",
	}

	var offenders []string
	checked := 0
	for _, layer := range layers {
		err := filepath.WalkDir(layer, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil //nolint:nilerr // an unreadable dir is not a finding
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil
			}
			src := stripComments(string(b))
			// Only files that actually write to OpenSearch are relevant.
			if !strings.Contains(src, "opensearchInit.OpenSearchClient") {
				return nil
			}
			checked++
			if rawBodyRe.MatchString(src) {
				offenders = append(offenders, p)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", layer, err)
		}
	}

	if checked == 0 {
		t.Fatal("found no OpenSearch callers outside models/; this test has gone stale " +
			"and is no longer checking anything")
	}
	if len(offenders) > 0 {
		t.Errorf("these files outside models/ write raw marshalled JSON to OpenSearch "+
			"instead of going through openSearchStruct.IndexReader / IndexBody:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// TestBulkSeamIsActuallyUsed mirrors TestIndexSeamIsActuallyUsed for the bulk path:
// the ratchet above would also pass if BulkReader were simply dropped, so pin that
// the bulk write actually uses it.
func TestBulkSeamIsActuallyUsed(t *testing.T) {
	for path, src := range modelSourceFiles(t) {
		if !strings.Contains(path, "Bulk") {
			continue
		}
		clean := stripComments(src)
		if !strings.Contains(clean, "BulkReader(") {
			t.Fatalf("%s performs a bulk write without going through "+
				"openSearchStruct.BulkReader, so batched writes are unguarded", path)
		}
		return
	}
	t.Fatal("the bulk model source was not found; this test has gone stale")
}

// TestIndexSeamIsActuallyUsed is the other half. Without it the ratchet above would
// also pass if every model stopped writing to OpenSearch altogether, or if someone
// "fixed" a failure by deleting the call rather than routing it through the seam.
func TestIndexSeamIsActuallyUsed(t *testing.T) {
	users := 0
	for _, src := range modelSourceFiles(t) {
		clean := stripComments(src)
		if strings.Contains(clean, "IndexReader(") || strings.Contains(clean, "IndexBody(") {
			users++
		}
	}
	if users < 8 {
		t.Fatalf("expected the index seam to be used across the model packages; only %d files use it", users)
	}
}
