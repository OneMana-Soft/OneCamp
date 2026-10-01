package helpers

// The base image a customer runs must be on a supported Go line.
//
// WHY A TEST AND NOT A NOTE. distribute-Dockerfile sat on golang:1.24-alpine for
// long enough that the line went end of life, which means the image and every
// Alpine package under it stopped receiving fixes. Nothing anywhere noticed,
// because the file is correct-looking on its own and nobody re-reads a FROM line.
//
// This does NOT check the Go version the binary was compiled with. It cannot:
// the binary is built by the release service before the archive is made, and this
// image only runs it. Conflating the two is the mistake that produced a
// confident, wrong answer during the audit that led to this file.
//
// The rule is deliberately loose. It asserts the base is not older than the go
// directive's line, which is the property that goes wrong silently. A newer base
// is fine and needs no permission.

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

var (
	dockerGoBase   = regexp.MustCompile(`(?m)^FROM\s+golang:(\d+)\.(\d+)`)
	goModDirective = regexp.MustCompile(`(?m)^go\s+(\d+)\.(\d+)`)
)

func TestCustomerBaseImageIsOnASupportedGoLine(t *testing.T) {
	df, err := os.ReadFile("../distribute-Dockerfile")
	if err != nil {
		t.Fatalf("reading the customer Dockerfile: %v", err)
	}
	gomod, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}

	base := dockerGoBase.FindSubmatch(df)
	if base == nil {
		// A non-golang base is the better answer, not a failure: this image only
		// needs to run a prebuilt binary. If somebody has moved it to plain
		// alpine or distroless, there is nothing here to check.
		t.Skip("the customer image no longer uses a golang base, which is an improvement")
	}
	mod := goModDirective.FindSubmatch(gomod)
	if mod == nil {
		t.Fatal("go.mod has no go directive")
	}

	baseMajor, baseMinor := atoi(t, base[1]), atoi(t, base[2])
	modMajor, modMinor := atoi(t, mod[1]), atoi(t, mod[2])

	if baseMajor < modMajor || (baseMajor == modMajor && baseMinor < modMinor) {
		t.Errorf("the customer base image is golang:%d.%d but go.mod declares go %d.%d.\n"+
			"A base older than the module's own line is one nobody is patching: Go supports\n"+
			"the two most recent majors, so the image and its Alpine packages stop receiving\n"+
			"fixes. Move the FROM line to at least %d.%d.",
			baseMajor, baseMinor, modMajor, modMinor, modMajor, modMinor)
	}
}

func atoi(t *testing.T, b []byte) int {
	t.Helper()
	n, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatalf("parsing %q: %v", b, err)
	}
	return n
}
