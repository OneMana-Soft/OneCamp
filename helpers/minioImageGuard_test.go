package helpers

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// MinIO's image must be one a server can still pull, and it must run as the user that owns the
// data it serves.
//
// WHAT WENT WRONG. Both stacks pinned minio/minio:RELEASE.2025-01-18T00-31-37Z. MinIO stopped
// publishing images in October 2025 and archived the project in 2026. Docker Hub has since dropped
// the minio/minio and minio/mc repositories (404 on 14 Oct 2026), and Quay's copies answer 401
// without a login. Nothing here noticed, because a pinned tag is exactly what
// TestNoComposeImageFloats asks for. Servers with the image cached kept running, and every new one
// failed `make install` before anything started.
//
// The replacement is Chainguard's image of the maintained fork (github.com/chainguard-forks/minio).
// It differs from the old image in two ways that matter here:
//   - Its free tier publishes only `latest`, so it is pinned by digest.
//   - It runs as uid 65532. Every install's ./data/minio was written as root by the old image, and
//     MinIO running as 65532 can't write there, so it doesn't start. The service runs as root, as
//     it always did, and the files need no migration.

// unpullableMinIO are repositories that no longer serve an anonymous pull. Naming one breaks every
// server that doesn't already have the image cached.
var unpullableMinIO = map[string]string{
	"minio/minio":         "Docker Hub dropped the repository (404 on 14 Oct 2026)",
	"minio/mc":            "Docker Hub dropped the repository (404 on 14 Oct 2026)",
	"quay.io/minio/minio": "Quay answers 401 without a login (14 Oct 2026)",
	"quay.io/minio/mc":    "Quay answers 401 without a login (14 Oct 2026)",
}

// dockerHubPrefix is what Docker reads as Docker Hub, so docker.io/minio/minio is minio/minio.
var dockerHubPrefix = regexp.MustCompile(`^(docker\.io|index\.docker\.io|registry-1\.docker\.io)/`)

func TestNoComposeNamesAMinIOImageThatCannotBePulled(t *testing.T) {
	var problems []string
	for _, s := range imageSites(t) {
		repo := dockerHubPrefix.ReplaceAllString(strings.ToLower(s.repository()), "")
		if why, ok := unpullableMinIO[repo]; ok {
			problems = append(problems, fmt.Sprintf("%s:%d (%s) names %s: %s", s.file, s.line, s.service, s.ref, why))
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("a compose file names a MinIO image that can't be pulled any more, so a new server fails "+
			"`make install` and an update fails `docker compose up`:\n  %s\nUse the pinned "+
			"cgr.dev/chainguard/minio digest the other stacks use.", strings.Join(problems, "\n  "))
	}
}

// minioStacks are the compose files that run the object store.
var minioStacks = []string{"../distribute-compose.yml", "../final-compose.yml"}

var (
	minioDigestRef = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	minioImageLine = regexp.MustCompile(`(?m)^\s+image:\s*"?([^"\s]+)"?\s*$`)
	minioRootUser  = regexp.MustCompile(`(?m)^\s+user:\s*"?(0|0:0|root|root:root)"?\s*$`)
)

// serviceLines returns one service's lines from a compose file. It ends at the next service key or
// top-level key, matched by composeServiceLine, which also matches a key with a trailing comment.
// The service after minio in distribute-compose.yml has one, and a pattern without the comment ran
// on into it.
func serviceLines(body, service string) string {
	var block []string
	in := false
	for _, line := range strings.Split(body, "\n") {
		if m := composeServiceLine.FindStringSubmatch(line); m != nil {
			in = m[1] == service
			continue
		}
		if composeTopLevelKey.MatchString(line) {
			in = false
		}
		if in {
			block = append(block, line)
		}
	}
	return strings.Join(block, "\n")
}

// minioServices returns each stack's minio service block, by file name. A stack that is absent
// (the demo's final-compose.yml in a carve) is skipped. One that is present without a minio
// service fails, since the checks below would then check nothing.
func minioServices(t *testing.T) map[string]string {
	t.Helper()
	blocks := map[string]string{}
	for _, file := range minioStacks {
		raw, err := os.ReadFile(file)
		if err != nil {
			if strings.Contains(file, "final-compose") && os.IsNotExist(err) {
				continue
			}
			t.Fatalf("reading %s: %v", file, err)
		}
		block := serviceLines(string(raw), "minio")
		if strings.TrimSpace(block) == "" {
			t.Fatalf("%s has no minio service, so these checks watch nothing", file)
		}
		blocks[filepath.Base(file)] = block
	}
	if len(blocks) == 0 {
		t.Fatal("no compose file runs MinIO, so these checks watch nothing")
	}
	return blocks
}

// minioImage is the image a minio service block names.
func minioImage(t *testing.T, file, block string) string {
	t.Helper()
	m := minioImageLine.FindStringSubmatch(block)
	if m == nil {
		t.Fatalf("%s: the minio service names no image", file)
	}
	return m[1]
}

// Chainguard's free tier publishes only `latest`, which moves every few days: pinned by tag, the
// object store would change under an install without anyone deciding it should. And root, because
// root wrote the data.
func TestMinIOIsPinnedByDigestAndRunsAsRoot(t *testing.T) {
	for file, block := range minioServices(t) {
		if ref := minioImage(t, file, block); !minioDigestRef.MatchString(ref) {
			t.Errorf("%s: the minio service names %s. Pin it by digest (@sha256:...): the image's free "+
				"tier publishes only `latest`, so no tag names one build", file, ref)
		}
		if !minioRootUser.MatchString(block) {
			t.Errorf("%s: the minio service doesn't run as root (user: \"0:0\"). The image's own user is "+
				"uid 65532. Every existing install's ./data/minio was written as root by the old image, so "+
				"MinIO as 65532 can't write its own data and doesn't start", file)
		}
	}
}

// The integration tests that start a real MinIO must start the one customers run, or a passing test
// says nothing about the shipped object store. Bump the compose files and the tests together.
func TestIntegrationTestsRunTheMinIOCustomersRun(t *testing.T) {
	shipped := map[string]bool{}
	for file, block := range minioServices(t) {
		shipped[minioImage(t, file, block)] = true
	}
	var want []string
	for ref := range shipped {
		want = append(want, ref)
	}
	sort.Strings(want)

	tests, err := filepath.Glob("../tests/integration/*.go")
	if err != nil {
		t.Fatal(err)
	}
	pinned := regexp.MustCompile(`cgr\.dev/chainguard/minio(?::[A-Za-z0-9._-]+)?@sha256:[0-9a-f]{64}`)
	for _, path := range tests {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, ref := range pinned.FindAllString(string(raw), -1) {
			if !shipped[ref] {
				t.Errorf("%s starts %s, but the compose files ship %s. Pin the test to the shipped "+
					"digest, or bump both together", filepath.Base(path), ref, strings.Join(want, ", "))
			}
		}
	}
}
