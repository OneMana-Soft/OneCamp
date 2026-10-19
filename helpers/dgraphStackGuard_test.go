package helpers

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// How the deployed stacks run Dgraph (DGRAPH-LEAD DG0, design/dgraph/REPORT-2026-10-18.md §2, owner
// rulings O-DG2 and O-DG4).
//
// WHY A FLOOR, AND WHY THIS ONE. Every advisory below was published against a release OneCamp ran,
// and each is closed only by running the release that fixed it. Quoted from Dgraph's CHANGELOG and
// its GitHub security advisories (read 18 Oct 2026):
//
//   - GHSA-rrwh-6jrq-wp5v / CVE-2026-54061, critical, fixed in v25.3.5: "Alpha exposed the
//     external-snapshot import RPCs on the public gRPC port with no authorization and no arming
//     requirement, letting an unauthenticated client wipe a group's store." That is port 9080, the
//     port the product itself uses; Alpha's --security whitelist governs the admin HTTP endpoints,
//     not gRPC, so every container on the shared network could do it.
//   - GHSA-q2m9-6jp9-c6mc / CVE-2026-44840, high, fixed in v25.3.4: DQL injection via the
//     checkUserPassword GraphQL query.
//   - GHSA-33p8-wc97-5qcj / CVE-2026-63637, high, fixed in v25.3.8: "DQL injection via the
//     unvalidated GraphQL `regexp` filter argument, exploitable unauthenticated in the default
//     configuration." OneCamp never uses GraphQL, but Alpha serves /graphql on 8080 regardless.
//   - GHSA-qj98-gp63-54xj / CVE-2026-102748, critical, fixed in v25.4.0: "unauthenticated Zero admin
//     HTTP endpoints allowed a control-plane denial of service via `/removeNode`."
//   - GHSA-vp52-pcj8-j9qc / CVE-2026-84304 (grpc-go), fixed in v25.5.0: "a heap memory exhaustion via
//     HTTP/2 DATA frame fragmentation that affects grpc-go 1.83.0 and earlier, including the 1.82.1
//     shipped in v25.4.x".
//
// So the floor is v25.5.0. Lowering it is a decision to run a known hole, and belongs in this list
// with its reason, not in a compose file.
var dgraphServerFloor = [3]int{25, 5, 0}

// dgraphVersion reads "v25.5.0" as {25, 5, 0}.
var dgraphVersion = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// composeServiceBlocks returns each service's lines, by service, for one compose file: everything
// from the service's key to the next service or top-level key. Comment lines are dropped, so a flag
// mentioned in a comment is neither required nor forbidden.
func composeServiceBlocks(t *testing.T, path string) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	blocks := map[string][]string{}
	service, inServices := "", false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "services:") {
			inServices = true
			continue
		}
		if inServices && composeTopLevelKey.MatchString(line) {
			inServices, service = false, ""
		}
		if !inServices {
			continue
		}
		if m := composeServiceLine.FindStringSubmatch(line); m != nil {
			service = m[1]
			continue
		}
		if service == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		blocks[service] = append(blocks[service], line)
	}
	return blocks
}

// dgraphStacks are the stacks that run Dgraph: the one customers install and the one beta and the
// demo run. Named rather than globbed, because the claims below are about these two and a glob that
// stopped matching would assert nothing.
var dgraphStacks = []string{"../distribute-compose.yml", "../final-compose.yml"}

// dgraphService finds the service in blocks whose command runs `dgraph <role>`, failing when there is
// none, so a renamed service can't make these checks vacuous.
func dgraphService(t *testing.T, path string, blocks map[string][]string, role string) (string, string) {
	t.Helper()
	cmd := regexp.MustCompile(`^\s+command:\s*dgraph\s+` + role + `\b(.*)$`)
	for name, lines := range blocks {
		for _, l := range lines {
			if m := cmd.FindStringSubmatch(l); m != nil {
				return name, m[1]
			}
		}
	}
	t.Fatalf("%s: no service runs `dgraph %s`; the Dgraph checks would look at nothing", path, role)
	return "", ""
}

func TestDgraphServerIsAtLeastTheAdvisoryFloor(t *testing.T) {
	floor := fmt.Sprintf("v%d.%d.%d", dgraphServerFloor[0], dgraphServerFloor[1], dgraphServerFloor[2])
	seen := 0
	for _, s := range imageSites(t) {
		if s.repository() != "dgraph/dgraph" {
			continue
		}
		seen++
		v, _ := s.version()
		m := dgraphVersion.FindStringSubmatch(v)
		if m == nil {
			t.Errorf("%s:%d %s: %q is not an exact vMAJOR.MINOR.PATCH, so it can't be held to the "+
				"advisory floor %s", s.file, s.line, s.service, v, floor)
			continue
		}
		var got [3]int
		for i := range got {
			got[i], _ = strconv.Atoi(m[i+1])
		}
		if slices.Compare(got[:], dgraphServerFloor[:]) < 0 {
			t.Errorf("%s:%d %s runs dgraph/dgraph:%s, below %s: it carries the advisories listed "+
				"on dgraphServerFloor", s.file, s.line, s.service, v, floor)
		}
	}
	if seen < 4 {
		t.Fatalf("found %d dgraph/dgraph image(s); expected zero and alpha in both stacks", seen)
	}
}

// Dgraph's built-in MCP server is never switched on (owner ruling O-DG4).
//
// `dgraph alpha --mcp` mounts /mcp/sse (read and write: run_query, run_mutation, alter_schema) and
// /mcp-ro/sse on Alpha's HTTP port, with no authentication of its own and no per-namespace scope:
// the whole graph, to whoever reaches 8080. Agents reach OneCamp through /v1/mcp, where every call
// goes through the agent's kill switch, the token's scopes and the member's own access
// (business/MCPServer/authorize.go). `dgraph mcp`, the stdio form, is the same surface.
//
// --expose_trace is refused for a related reason: it serves request traces over HTTP, and v25.5.0
// deprecates it ahead of rejecting it.
func TestNoComposeFileTurnsOnDgraphsOwnMCPOrTraces(t *testing.T) {
	paths, err := filepath.Glob("../*compose*.yml")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) < 2 {
		t.Fatalf("found %d compose file(s); this guard would look at nothing", len(paths))
	}
	forbidden := regexp.MustCompile(`(--mcp\b|\bdgraph\s+mcp\b|--expose_trace)`)
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if m := forbidden.FindString(line); m != "" {
				t.Errorf("%s:%d passes %q: Dgraph's own MCP server and traces are unauthenticated "+
					"windows on the whole graph; agents use /v1/mcp", filepath.Base(path), i+1, m)
			}
		}
	}
}

// Zero and Alpha both refuse callers outside the private ranges, and Alpha is unreachable from the
// public proxy.
//
// ZERO. Until v25.4.0 Zero's admin HTTP endpoints (6080) had no authentication at all. v25.4.0 gave
// Zero a --security superflag; without it, /removeNode and /moveTablet admit only loopback but /state
// and /assign stay open. A whitelist closes all four to anything outside the Docker networks.
//
// ALPHA ON traefik-public. distribute-compose.yml put Alpha on the proxy's network for a Traefik
// router that published its HTTP port; e0236562 (12 Aug 2026) removed the router and left the
// network. Nothing routes to Alpha through Traefik and nothing on that network needs it: the API and
// the worker reach it as alpha:9080 over onecamp-shared-net, and backup and restore run inside the
// container. The network only gave the internet-facing proxy (and the web container, which is on
// traefik-public alone) a path to /admin, /alter and the gRPC port.
func TestDgraphIsFencedToThePrivateNetwork(t *testing.T) {
	openWhitelist := regexp.MustCompile(`whitelist=[^"';\s]*0\.0\.0\.0/0`)
	for _, path := range dgraphStacks {
		blocks := composeServiceBlocks(t, path)
		file := filepath.Base(path)
		for _, role := range []string{"zero", "alpha"} {
			name, args := dgraphService(t, path, blocks, role)
			if !strings.Contains(args, "--security") || !strings.Contains(args, "whitelist=") {
				t.Errorf("%s: %s (dgraph %s) has no --security whitelist; its admin endpoints answer "+
					"anyone who can reach them", file, name, role)
			}
			if openWhitelist.MatchString(args) {
				t.Errorf("%s: %s (dgraph %s) whitelists 0.0.0.0/0, which is no whitelist", file, name, role)
			}
		}
		alpha, _ := dgraphService(t, path, blocks, "alpha")
		for _, l := range blocks[alpha] {
			if strings.TrimSpace(l) == "- traefik-public" {
				t.Errorf("%s: %s is on traefik-public; nothing routes to it there, and the network gives "+
					"the public proxy a path to Dgraph's admin and gRPC ports", file, alpha)
			}
		}
	}
}

// Alpha's memory is bounded, and its cache is sized rather than left to the default.
//
// v25.1.0 raised Alpha's default cache from 1 GB to 4 GB ("the default cacheMB went from 1 GB to
// 4 GB"), which is the whole of a 4 GB server, and Alpha was the one database service in these
// stacks with no memory limit. An explicit --cache size-mb keeps the cache to what the server was
// sized for, and mem_limit keeps a runaway query from taking the machine (the API, Postgres and
// OpenSearch with it) instead of only restarting Alpha.
func TestDgraphAlphaMemoryIsBounded(t *testing.T) {
	cache := regexp.MustCompile(`--cache\s+"?size-mb=`)
	limit := regexp.MustCompile(`^\s+mem_limit:\s*\S+`)
	for _, path := range dgraphStacks {
		blocks := composeServiceBlocks(t, path)
		file := filepath.Base(path)
		alpha, args := dgraphService(t, path, blocks, "alpha")
		if !cache.MatchString(args) {
			t.Errorf("%s: %s has no --cache size-mb; since v25.1.0 the default is 4 GB", file, alpha)
		}
		limited := false
		for _, l := range blocks[alpha] {
			if limit.MatchString(l) {
				limited = true
			}
		}
		if !limited {
			t.Errorf("%s: %s has no mem_limit", file, alpha)
		}
	}
}

// Ratel, where a stack runs it, is the newest published release (it administers the v25 server it is
// paired with). Kept separate from the server floor: Ratel is a browser client with no advisories of
// its own, so this only stops it drifting behind.
func TestRatelIsTheCurrentRelease(t *testing.T) {
	const want = "v25.3.0"
	var bad []string
	for _, s := range imageSites(t) {
		if s.repository() != "dgraph/ratel" {
			continue
		}
		if v, _ := s.version(); v != want {
			bad = append(bad, fmt.Sprintf("%s:%d %s", s.file, s.line, v))
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("dgraph/ratel at %s; expected %s", b, want)
	}
}

// The integration harness runs the Dgraph the stacks ship, so a test passes against the server
// customers get. It used dgraph/dgraph:latest, which named whatever a machine last pulled: v25.4.1 on
// the build workstation on 18 Oct 2026, while both stacks ran v25.3.3.
func TestIntegrationHarnessRunsTheShippedDgraph(t *testing.T) {
	raw, err := os.ReadFile("../tests/integration/dgraph.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`"dgraph/dgraph:([^"]+)"`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("tests/integration/dgraph.go names no dgraph/dgraph image; this guard would look at nothing")
	}
	for _, s := range imageSites(t) {
		if s.repository() != "dgraph/dgraph" {
			continue
		}
		if v, _ := s.version(); v != m[1] {
			t.Errorf("the integration harness runs dgraph/dgraph:%s but %s:%d (%s) ships %s",
				m[1], s.file, s.line, s.service, v)
		}
	}
}

// Neither Zero nor Alpha reports telemetry. Both default to `reports=true` ("Send anonymous telemetry
// data to Dgraph devs", dgraph zero/alpha --help, v25.5.0), and a self-hosted install sends nothing
// anywhere it wasn't asked to.
func TestDgraphSendsNoTelemetry(t *testing.T) {
	off := regexp.MustCompile(`--telemetry\s+"[^"]*reports=false`)
	for _, path := range dgraphStacks {
		blocks := composeServiceBlocks(t, path)
		for _, role := range []string{"zero", "alpha"} {
			name, args := dgraphService(t, path, blocks, role)
			if !off.MatchString(args) {
				t.Errorf("%s: %s (dgraph %s) doesn't pass --telemetry \"reports=false\"; Dgraph reports "+
					"usage by default", filepath.Base(path), name, role)
			}
		}
	}
}
