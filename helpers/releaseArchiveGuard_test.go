package helpers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What a buyer downloads, and where each piece comes from.
//
// The archive is assembled in a DIFFERENT REPOSITORY. onemana-backend's
// DownloadLatestZip walks this repo and copies these paths in, renaming most of
// them on the way: the file called Makefile-distribute here is the Makefile the
// customer runs, distribute-compose.yml becomes the sample-compose.yml their
// first command copies, and vars/.env.prod becomes the .sample.env every other
// target reads.
//
// Nothing connected the two sides. Renaming or moving any of these here is a
// legal, green, reviewable change that breaks the download in a repository this
// one does not import and cannot see. Each copy fails closed, so the archive is
// never silently incomplete, but the failure surfaces at the worst moment
// available: after payment, to the buyer, as a download that does not work.
//
// Keep this table equal to the sequence of addFilesToZip calls in
// onemana-backend/business/onecamp/onecampBusiness.go. "app" is excluded on
// purpose: it is the compiled binary, produced by the build rather than read
// from here.
var releaseArchive = map[string]string{
	"distribute-Dockerfile":  "Dockerfile",
	"distribute-compose.yml": "sample-compose.yml",
	".dockerignore":          ".dockerignore",
	"Makefile-distribute":    "Makefile",
	"migrations":             "migrations",
	"vars/.env.prod":         ".sample.env",
	"livekit.yaml.sample":    "livekit.yaml.sample",
	"ch-docker":              "ch-docker",
	"emqx":                   "emqx",
	"livekit-agent":          "livekit-agent",
	"other-services":         "other-services",
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// The guards in this package all run from helpers/.
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("cannot resolve the repository root: %v", err)
	}
	return root
}

// Every path the packaging step reads must still be here.
func TestReleaseArchiveSourcesExist(t *testing.T) {
	root := repoRoot(t)
	for src, dst := range releaseArchive {
		if _, err := os.Stat(filepath.Join(root, src)); err != nil {
			t.Errorf(
				"%s is missing, so every customer download fails while assembling %q.\n"+
					"If it moved, update this table AND the addFilesToZip call in "+
					"onemana-backend/business/onecamp/onecampBusiness.go.",
				src, dst)
		}
	}
}

// The names the shipped Makefile bootstraps from must be names the archive
// actually contains.
//
// `make install` begins by copying .sample.env to .env and sample-compose.yml
// to compose.yml. Those two names exist only inside the archive: grep this repo
// for either and you find nothing, which is exactly why a rename on the
// packaging side would look harmless from here.
func TestShippedMakefileBootstrapsFromFilesTheArchiveContains(t *testing.T) {
	root := repoRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "Makefile-distribute"))
	if err != nil {
		t.Fatalf("the shipped Makefile is unreadable: %v", err)
	}
	text := string(body)

	shipped := map[string]bool{}
	for _, name := range releaseArchive {
		shipped[name] = true
	}

	// Named individually rather than parsed out of the Makefile: a regex over a
	// Makefile finds every word with a dot in it, and a guard that reports
	// twenty false positives is one somebody deletes.
	for _, needed := range []string{".sample.env", "sample-compose.yml", "livekit.yaml.sample"} {
		if !strings.Contains(text, needed) {
			// Not a failure of the archive. It means the Makefile stopped using
			// a file we still ship, which is worth knowing but is not a break.
			t.Logf("note: the shipped Makefile no longer mentions %s", needed)
			continue
		}
		if !shipped[needed] {
			t.Errorf(
				"the shipped Makefile bootstraps from %q, which the archive does not contain.\n"+
					"A customer's first command fails. Fix the mapping in "+
					"onemana-backend/business/onecamp/onecampBusiness.go.",
				needed)
		}
	}
}

// The env template a customer starts from must not carry this deployment's own
// values, and must not have quietly stopped being a template.
func TestShippedEnvTemplateIsATemplate(t *testing.T) {
	root := repoRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "vars/.env.prod"))
	if err != nil {
		t.Fatalf("the env template shipped as .sample.env is unreadable: %v", err)
	}
	text := string(body)

	// One real hostname here is one install's address handed to everybody. The
	// same class the frontend guards against, on the file a buyer edits first.
	for _, ours := range []string{"onemana.dev", "139.99.122.240"} {
		if strings.Contains(text, ours) {
			t.Errorf("vars/.env.prod names %q, so every buyer starts configured to point at our deployment", ours)
		}
	}
	if strings.TrimSpace(text) == "" {
		t.Fatal("vars/.env.prod is empty, so .sample.env gives a buyer nothing to fill in")
	}
}
