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

// No compose file may name an image whose contents can change without anyone deciding they should.
//
// WHY, MEASURED RATHER THAN ASSUMED. THIRTY of the fifty-one image references across the compose files
// were floating: `:latest`, or in one case no tag at all. Checking the eight distinct upstream images
// against Docker Hub found that `latest` had ALREADY MOVED for every single one of them — beta was
// running Redis 8.6.2 while `latest` had reached 8.6.5, Dgraph v25.3.3, EMQX 6.0.0, LiveKit v1.11.0.
// So the deployed stack was a set of versions nobody chose, recorded nowhere, and a `docker compose
// pull` aimed at any of them would have moved a datastore under a live workspace.
//
// WORSE, AND THIS IS THE PART THAT MADE IT URGENT. For three of the eight — ratel, opensearch and
// opensearch-dashboards — NO published tag matched the running image's digest at all. Upstream had
// re-pushed or pruned it. The running configuration was not reproducible from any tag in existence: if
// those containers had been removed, what came back would necessarily have been something else.
//
// AND THE SHIPPED STACK DIFFERED FROM THE TESTED ONE. distribute-compose.yml named `minio/minio` with
// no tag while final-compose.yml pinned a RELEASE. Docker reads a missing
// tag as `latest`, so object storage was one version on the two stacks under test and a floating,
// different one on the stack customers receive. Nothing in review shows that, because the line looks
// like an image reference and is.
//
// Every pin was established by matching the RUNNING image's manifest digest against the repository's
// published tags, not by reading a version label — a label says what upstream built, a digest says what
// is executing. Where no tag matched, the compose file says so inline, because the pin is then a
// version choice rather than a record.
//
// The tags here are deliberately versions and not digests. A digest is immutable, which is the argument
// for it, but it is also unreadable at a glance and it breaks when upstream prunes it — a loud failure
// in a customer's install. Versions are legible in review, reproducible in practice, and the thing an
// operator actually reasons about. tinyproxy is pinned by digest precisely because it is the exception:
// upstream publishes exactly one tag, so there is no version to name.
const (
	// floatingLatest is the tag Docker assumes when none is given, which is why a bare `image: foo/bar`
	// is the same fault as writing it out.
	floatingLatest = "latest"

	// minImageRefs is the "am I scanning anything" floor. Fifty-one references exist today across six
	// files; twenty is low enough to survive a stack being retired and high enough that a pattern which
	// has stopped matching fails here rather than passing vacuously.
	minImageRefs = 20
)

var (
	composeImageLine = regexp.MustCompile(`^\s+image:\s*"?([^"\s]+)"?\s*$`)
	composeBuildKey  = regexp.MustCompile(`^\s+build:\s*$`)

	// ${VAR:-default} — the default is what has to be checked, since it is what applies when nobody
	// sets the variable, which is the normal case.
	varWithDefault = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*:-(.*)\}$`)
	// ${VAR} with no default: unset interpolates to empty and compose rejects the reference outright, so
	// the environment is genuinely required to supply it. Pinned, just not here.
	varNoDefault = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)
)

// locallyBuiltImages are exempt, and the reason is not "we could not be bothered".
//
// These are built from a Dockerfile in this repository by the compose file that names them. Nothing
// pulls them from a registry, so there is no upstream that can change them: the bytes move only when
// somebody runs a build, which is a deliberate act with a diff attached. That is a different risk from
// a tag another organisation controls, and it is the only reason an exemption is defensible.
//
// The exemption is not taken on trust — TestFloatingImageExemptionsAreRealBuilds asserts each of these
// services actually declares `build:`, so the list cannot be used to park a pulled image.
//
// STILL WORTH DOING, SEPARATELY: go_service_onecamp:1.0 shows the house convention is to version its
// own artefacts, and these two do not follow it, so "which build is running" has no answer. Fixing that
// means versioning the build targets, which is a change to how releases are cut rather than to a tag.
var locallyBuiltImages = map[string]string{
	"onecamp/code-runner":        "built from other-services/code-runner by the compose file that names it",
	"onecamp/code-runner-coding": "built from other-services/code-runner by the coding overlay",
}

// imageSite is one image reference, kept with enough context for a failure to name a line to open.
type imageSite struct {
	file     string
	line     int
	service  string
	ref      string
	hasBuild bool
}

// splitRef separates an image reference into repository and version (tag or digest).
//
// The colon scan has to ignore anything inside ${...}, and that is not fussiness. The first version of
// this used strings.LastIndex(ref, ":"), which on
//
//	opensearchproject/opensearch:${OPENSEARCH_VERSION:-latest}
//
// finds the colon in the VARIABLE and returns "-latest}" as the tag. That parses as neither `latest` nor
// a variable, so the reference read as pinned while defaulting to latest — the exact shape this guard
// exists to catch, waved through by the guard. Caught by negative-verifying the check rather than by it
// passing, which is the only way that class of bug surfaces.
//
// A colon before the last slash is a registry port (registry:5000/image), not a tag.
func splitRef(ref string) (repo, version string, hasVersion bool) {
	if at := strings.Index(ref, "@"); at >= 0 {
		return ref[:at], ref[at+1:], true
	}

	start := strings.LastIndex(ref, "/") + 1
	depth := 0
	for i := start; i < len(ref); i++ {
		switch {
		case strings.HasPrefix(ref[i:], "${"):
			depth++
			i++ // skip the brace so it is not re-read
		case ref[i] == '}' && depth > 0:
			depth--
		case ref[i] == ':' && depth == 0:
			return ref[:i], ref[i+1:], true
		}
	}
	return ref, "", false
}

// repository returns the image name without its tag or digest.
func (s imageSite) repository() string {
	repo, _, _ := splitRef(s.ref)
	return repo
}

// version returns the tag or digest, and whether one was written at all.
func (s imageSite) version() (string, bool) {
	_, v, ok := splitRef(s.ref)
	return v, ok
}

// imageSites returns every image reference across every compose file in the repository.
//
// Globs rather than listing files, on purpose and unlike composeRestartGuard_test.go: that guard makes a
// claim about a known set of deployed stacks, so a glob matching nothing would silently assert nothing.
// The claim here is "wherever an image is named, it is pinned", which has to cover a compose file nobody
// has written yet. minImageRefs is what stops an empty glob passing.
//
// Reuses composeServiceLine from composeRestartGuard_test.go rather than declaring a third copy of the
// same pattern. That matters: the two copies that existed had the same blind spot, and a service key
// with a trailing comment was invisible to both.
func imageSites(t *testing.T) []imageSite {
	t.Helper()

	paths, err := filepath.Glob("../*compose*.yml")
	if err != nil {
		t.Fatalf("globbing compose files: %v", err)
	}
	sort.Strings(paths)

	var sites []imageSite
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}

		service, hasBuild, inServices := "", false, false
		pending := map[string]*imageSite{}
		for i, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "services:") {
				inServices = true
				continue
			}
			if inServices && composeTopLevelKey.MatchString(line) {
				inServices = false
			}
			if !inServices {
				continue
			}
			if m := composeServiceLine.FindStringSubmatch(line); m != nil {
				service, hasBuild = m[1], false
				continue
			}
			if service == "" {
				continue
			}
			if composeBuildKey.MatchString(line) {
				hasBuild = true
				if s, ok := pending[service]; ok {
					s.hasBuild = true
				}
			}
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if m := composeImageLine.FindStringSubmatch(line); m != nil {
				sites = append(sites, imageSite{
					file: filepath.Base(path), line: i + 1,
					service: service, ref: m[1], hasBuild: hasBuild,
				})
				pending[service] = &sites[len(sites)-1]
			}
		}
	}

	if len(sites) < minImageRefs {
		t.Fatalf("found %d image reference(s) across ../*compose*.yml, expected at least %d. Either the "+
			"images moved out of compose or this pattern no longer matches them; a check that scans "+
			"nothing passes for the wrong reason", len(sites), minImageRefs)
	}
	return sites
}

// isFloating reports whether a reference can change contents without a decision, and why.
func isFloating(s imageSite) (bool, string) {
	v, ok := s.version()
	if !ok {
		return true, "no tag at all, which Docker reads as :latest"
	}
	if v == floatingLatest {
		return true, "literal :latest"
	}
	if m := varWithDefault.FindStringSubmatch(v); m != nil {
		if m[1] == floatingLatest || m[1] == "" {
			return true, fmt.Sprintf("variable defaulting to %q, which applies whenever it is unset", m[1])
		}
		return false, ""
	}
	if varNoDefault.MatchString(v) {
		// Unset interpolates to empty and compose refuses the reference, so the environment must supply
		// it. Pinned somewhere else, which is a legitimate arrangement.
		return false, ""
	}
	return false, ""
}

func TestNoComposeImageFloats(t *testing.T) {
	var problems []string
	exempted := 0

	for _, s := range imageSites(t) {
		floating, why := isFloating(s)
		if !floating {
			continue
		}
		if _, ok := locallyBuiltImages[s.repository()]; ok {
			exempted++
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"%s:%d (%s) names %s — %s", s.file, s.line, s.service, s.ref, why))
	}

	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("%d image reference(s) can change contents without anyone deciding they should. Every "+
			"one of the eight upstream tags previously left floating here had ALREADY moved by the time "+
			"it was measured, so this is not a theoretical property:\n  %s\n"+
			"Pin the version the host is actually running — match the running image's digest against the "+
			"repository's published tags rather than trusting a version label.",
			len(problems), strings.Join(problems, "\n  "))
	}
	if exempted == 0 {
		t.Error("no reference matched locallyBuiltImages, so that exemption list is now dead weight " +
			"pretending to be a policy. Remove it or correct it.")
	}
}

// An exemption for a locally built image must describe a locally built image.
func TestFloatingImageExemptionsAreRealBuilds(t *testing.T) {
	seen := map[string]bool{}

	for _, s := range imageSites(t) {
		repo := s.repository()
		if _, ok := locallyBuiltImages[repo]; !ok {
			continue
		}
		seen[repo] = true
		if !s.hasBuild {
			t.Errorf("%s:%d (%s) uses %s, which locallyBuiltImages exempts from pinning on the grounds "+
				"that it is built here — but this service declares no `build:`, so it is PULLED from a "+
				"registry and the exemption is false", s.file, s.line, s.service, s.ref)
		}
	}

	for repo, reason := range locallyBuiltImages {
		if !seen[repo] {
			t.Errorf("locallyBuiltImages exempts %q (%q) but no compose file names it. An exemption for "+
				"something that no longer exists is a claim nobody is checking", repo, reason)
		}
	}
}

// One image, one version, everywhere.
//
// distribute-compose.yml shipped an untagged minio while the two stacks under test pinned a RELEASE, so
// customers ran a different object store from the one anything was verified against. A per-file version
// is invisible in review because each line, read alone, looks right.
func TestComposeImageVersionsAgreeAcrossFiles(t *testing.T) {
	// repository -> version -> where it is written
	byRepo := map[string]map[string][]string{}

	for _, s := range imageSites(t) {
		if _, ok := locallyBuiltImages[s.repository()]; ok {
			continue
		}
		v, ok := s.version()
		if !ok {
			continue // already reported by TestNoComposeImageFloats
		}
		repo := s.repository()
		if byRepo[repo] == nil {
			byRepo[repo] = map[string][]string{}
		}
		byRepo[repo][v] = append(byRepo[repo][v], fmt.Sprintf("%s:%d", s.file, s.line))
	}

	var repos []string
	for repo := range byRepo {
		repos = append(repos, repo)
	}
	sort.Strings(repos)

	for _, repo := range repos {
		versions := byRepo[repo]
		if len(versions) <= 1 {
			continue
		}
		var detail []string
		var keys []string
		for v := range versions {
			keys = append(keys, v)
		}
		sort.Strings(keys)
		for _, v := range keys {
			sites := versions[v]
			sort.Strings(sites)
			detail = append(detail, fmt.Sprintf("%s <- %s", v, strings.Join(sites, ", ")))
		}
		t.Errorf("%s is pinned to %d different versions, so what a deployment runs depends on which "+
			"compose file started it:\n  %s\nThese files are the same product on different topologies; "+
			"a version that differs between them is a difference nothing tests.",
			repo, len(versions), strings.Join(detail, "\n  "))
	}
}

// pairedImages must carry the same version as each other, because the software says so.
//
// distribute-compose.yml has carried this instruction as a comment on the image line for as long as the
// file has existed — "Make sure the version of opensearch-dashboards matches the version of opensearch
// installed on other nodes" — and nothing enforced it. Dashboards refuses to run against a node of a
// different version, so the pair skewing is not a degradation, it is search administration gone.
//
// It became reachable rather than theoretical when OPENSEARCH_VERSION started applying to both: one knob
// moving two images is only safe while both images read the knob.
var pairedImages = [][2]string{
	{"opensearchproject/opensearch", "opensearchproject/opensearch-dashboards"},
}

func TestPairedImagesShareAVersion(t *testing.T) {
	versions := map[string]map[string][]string{}
	for _, s := range imageSites(t) {
		v, ok := s.version()
		if !ok {
			continue
		}
		repo := s.repository()
		if versions[repo] == nil {
			versions[repo] = map[string][]string{}
		}
		versions[repo][v] = append(versions[repo][v], fmt.Sprintf("%s:%d", s.file, s.line))
	}

	for _, pair := range pairedImages {
		left, right := versions[pair[0]], versions[pair[1]]
		if len(left) == 0 || len(right) == 0 {
			// One half is not deployed anywhere; nothing to pair.
			continue
		}
		for lv, lsites := range left {
			for rv, rsites := range right {
				if lv == rv {
					continue
				}
				sort.Strings(lsites)
				sort.Strings(rsites)
				t.Errorf("%s and %s must carry the same version and do not:\n  %s -> %s\n  %s -> %s\n"+
					"Dashboards refuses to run against a node of a different version, so this is not a "+
					"mismatch to tidy up later.",
					pair[0], pair[1], pair[0], lv+" @ "+strings.Join(lsites, ", "),
					pair[1], rv+" @ "+strings.Join(rsites, ", "))
			}
		}
	}
}
