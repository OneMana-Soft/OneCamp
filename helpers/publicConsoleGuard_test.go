package helpers

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// No management console may be published to the public internet.
//
// WHY, AND THIS WAS LIVE. A Traefik router on a service's management port makes that console reachable
// over HTTPS from anywhere, with nothing in front of it but whatever the console itself asks for. Five
// were published across the deployed stacks, and checking them against the running beta host rather
// than reasoning about them found:
//
//   - DGRAPH ALPHA, on its HTTP port, answered an UNAUTHENTICATED schema query and returned the full
//     predicate list. Its health output carried no `acl` in ee_features, so authorization was not
//     merely misconfigured, it was absent. And the container ran with
//     `--security whitelist=0.0.0.0/0`, which is the flag governing /alter, /admin, export and
//     drop_all — set to the entire internet. So the identity and permission graph of the workspace was
//     world-readable and, on the same surface, world-writable.
//   - RATEL, the Dgraph admin UI, served its dashboard on 200. It is the client for the above and has
//     no authentication of its own.
//   - THE EMQX DASHBOARD served its login on 200, and the password on that host was THREE characters.
//     That console creates broker users and ACLs and can publish on any topic, which is every realtime
//     message, presence event and notification in the workspace.
//   - The MinIO console and OpenSearch Dashboards were published too. Both at least have a login.
//
// None of them was reached through Traefik by the product. DGRAPH_HOST is `alpha:9080` and OS_HOST is
// the node's internal address, so every one of those routers existed solely for a human's convenience
// and was available to everybody else at the same time.
//
// WHY A TEST RATHER THAN JUST DELETING THEM. A router is a label, added one line at a time by whoever
// needed to look at something once, and it is invisible in review because it looks exactly like the
// labels that publish the product. Nothing about `loadbalancer.server.port=18083` reads as "the message
// broker's admin panel is now on the internet". The consoles are reachable through an SSH tunnel, which
// costs an operator one command and costs an attacker everything.
var (
	// A Traefik label naming the port a router forwards to. The port is what identifies a console;
	// router names are arbitrary and get renamed.
	//
	// `http|tcp` IS THE LOAD-BEARING PART. The first version matched only traefik.http, and the audit
	// built on it reported the consoles and missed that POSTGRES AND REDIS each had a traefik.tcp
	// router on a dedicated entrypoint — the database and the cache on the public internet behind
	// nothing but a six- and a seven-character lowercase password. I found them by reading `docker
	// compose config` output instead of the labels, which is the only reason they turned up at all. A
	// pattern that names one protocol silently exempts the other.
	traefikServicePort = regexp.MustCompile(
		`traefik\.(?:http|tcp)\.services\.[A-Za-z0-9${}_-]+\.loadbalancer\.server\.port=(\d+)`)

	// A Traefik entrypoint declaration on the proxy itself. An entrypoint is the listening socket, so
	// one on 5432 is an open database port whether or not a router currently points at it.
	traefikEntrypoint = regexp.MustCompile(`--entrypoints\.([A-Za-z0-9_-]+)\.address=:(\d+)`)

	// Which service a label block belongs to, so a failure names something findable.
	consoleGuardService = regexp.MustCompile(`^  ([a-z0-9][a-z0-9._-]*):\s*(?:#.*)?$`)
	consoleGuardTopKey  = regexp.MustCompile(`^[a-zA-Z]`)
)

// managementPorts are ports whose service is an administrative or data-plane surface rather than
// product traffic. Keyed by port because that is the durable fact: EMQX's dashboard is 18083 whatever
// the router is called this month.
var managementPorts = map[string]string{
	"18083": "EMQX dashboard — creates broker users and ACLs, and can publish on any topic",
	"8000":  "Dgraph Ratel — runs arbitrary queries and mutations against the permission graph",
	"5601":  "OpenSearch Dashboards — reads every indexed document in the workspace",
	"9200":  "OpenSearch REST API — direct, unmediated index access",
	"6080":  "Dgraph Zero — cluster membership and tablet moves",
	"7080":  "Dgraph Alpha internal gRPC",
	"9080":  "Dgraph Alpha gRPC — the API the product itself uses, and not for the internet",
	"11434": "Ollama — an unauthenticated model API",
	"9001":  "MinIO console — full object-storage administration on its default port",

	// Data stores. These were published over traefik.tcp, which the first version of this pattern did
	// not match at all.
	"5432": "Postgres — the primary database, holding everything",
	"6379": "Redis — sessions and cache, and a Redis reachable from outside is a Redis somebody can " +
		"flush",
	"7687":  "Bolt / graph protocol port",
	"27017": "MongoDB",
	"8123":  "ClickHouse HTTP",
	"9009":  "ClickHouse interserver",
}

// publiclyRoutablePorts are ports a browser or client legitimately reaches, with the reason. Listed
// explicitly so that publishing something new is a decision recorded here rather than a label nobody
// reads.
var publiclyRoutablePorts = map[string]string{
	"3000": "go-service — the product API, which is the point",
	"8083": "EMQX over WebSocket — browsers subscribe to realtime here",
	"1234": "collaboration-service — the realtime document and board socket",
	"9000": "MinIO S3 API — attachments are fetched from it directly, see MINIO_HOST",
	"8091": "HyperDX — an observability UI WITH its own login. Published deliberately; it is the one " +
		"console left on the public surface and it is the next candidate for removal",
	"5349": "LiveKit TURN over TLS — a relay only works if the peer that needs relaying can reach it, " +
		"so this one is public by definition rather than by convenience",
	"7880": "LiveKit signalling — the WebSocket a browser opens to join a call. Reached through " +
		"Traefik on 443, never directly; this is the backend port that router forwards to",
	"7881": "LiveKit ICE/TCP fallback — the media path for peers whose network blocks UDP. " +
		"Published directly because media is not HTTP and a reverse proxy cannot carry it",
	"3478": "STUN/TURN — how a peer discovers its own reachable address before a call can connect",
}

// PORT 8080 IS AMBIGUOUS, and a port-only classification cannot say so.
//
// Three different things in these files listen on 8080: Traefik's own dashboard, which sits behind the
// admin-auth basicauth middleware and is acceptable; Dgraph Alpha's HTTP API, which is /query, /mutate,
// /alter and drop_all; and the MinIO console, which this deployment moves off 9001 with
// `--console-address ":8080"`. Two of the three must never be published and one may.
//
// The first version of this guard keyed on the port alone, put 8080 in publiclyRoutablePorts for
// Traefik's sake, and special-cased the traefik service. Negative-verifying it caught the consequence:
// re-adding a router for Dgraph Alpha on 8080 PASSED, because 8080 was absent from managementPorts and
// so never reached the management branch at all. The guard would have waved through the single worst
// exposure it exists to prevent.
//
// So ambiguous cases are keyed on service AND port, and checked before the port-only maps.
var managementServicePorts = map[string]string{
	"alpha:8080": "Dgraph Alpha HTTP — /query, /mutate, /alter and drop_all on the permission graph",
	"minio:8080": "MinIO console — this deployment moves it here with --console-address",
}

// publicServicePorts are (service, port) pairs that are acceptable despite the port looking
// administrative. Narrow by construction: each entry has to name why.
var publicServicePorts = map[string]string{
	"traefik:8080": "Traefik's own dashboard, in distribute-compose.yml only, behind the admin-auth " +
		"basicauth middleware defined on the same service",
}

// consolePortSites is one published port, with where it is published from.
type consolePortSite struct {
	file    string
	line    int
	service string
	port    string
}

func publishedPorts(t *testing.T) []consolePortSite {
	t.Helper()

	var sites []consolePortSite
	for _, path := range deployedComposeFiles {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		service, inServices := "", false
		for i, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "services:") {
				inServices = true
				continue
			}
			if inServices && consoleGuardTopKey.MatchString(line) {
				inServices = false
			}
			if !inServices {
				continue
			}
			if m := consoleGuardService.FindStringSubmatch(line); m != nil {
				service = m[1]
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if m := traefikServicePort.FindStringSubmatch(line); m != nil {
				sites = append(sites, consolePortSite{
					file: shortName(path), line: i + 1, service: service, port: m[1],
				})
			}
		}
	}
	if len(sites) == 0 {
		t.Fatal("found no traefik service-port labels at all across the deployed compose files; the " +
			"pattern no longer matches them, so this check is asserting nothing")
	}
	return sites
}

// shortName trims the leading ../ so failures read as filenames.
func shortName(path string) string {
	return strings.TrimPrefix(path, "../")
}

func TestNoManagementConsoleIsPublished(t *testing.T) {
	var problems []string

	for _, s := range publishedPorts(t) {
		key := s.service + ":" + s.port

		// Service-and-port first, because it is the only thing that can resolve an ambiguous port.
		if _, ok := publicServicePorts[key]; ok {
			continue
		}
		if why, ok := managementServicePorts[key]; ok {
			problems = append(problems, fmt.Sprintf(
				"%s:%d publishes %s on port %s — %s", s.file, s.line, s.service, s.port, why))
			continue
		}
		if why, ok := managementPorts[s.port]; ok {
			problems = append(problems, fmt.Sprintf(
				"%s:%d publishes %s on port %s — %s", s.file, s.line, s.service, s.port, why))
		}
	}

	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("%d management console(s) are routed to the public internet. Every one previously "+
			"published here was reachable in production, and two answered with no authentication at "+
			"all:\n  %s\nAn operator reaches these through an SSH tunnel. A router publishes them to "+
			"everybody at once.", len(problems), strings.Join(problems, "\n  "))
	}
}

// A port that is neither known-public nor known-management is a port nobody has classified.
//
// The point of this is that the list above stays honest. Without it, publishing a console on a port
// that simply is not in managementPorts would pass — which is the same silent-exemption failure that
// let a service with a trailing comment in its name escape the restart-policy guard.
func TestEveryPublishedPortIsClassified(t *testing.T) {
	unknown := map[string][]string{}

	for _, s := range publishedPorts(t) {
		key := s.service + ":" + s.port
		if _, ok := publicServicePorts[key]; ok {
			continue
		}
		if _, ok := managementServicePorts[key]; ok {
			continue
		}
		if _, ok := managementPorts[s.port]; ok {
			continue
		}
		if _, ok := publiclyRoutablePorts[s.port]; ok {
			continue
		}
		unknown[s.port] = append(unknown[s.port],
			fmt.Sprintf("%s:%d (%s)", s.file, s.line, s.service))
	}

	var ports []string
	for p := range unknown {
		ports = append(ports, p)
	}
	sort.Strings(ports)

	for _, p := range ports {
		sites := unknown[p]
		sort.Strings(sites)
		t.Errorf("port %s is published but classified neither as product traffic nor as a management "+
			"console: %s\nAdd it to publiclyRoutablePorts with the reason it belongs on the public "+
			"internet, or to managementPorts so it is refused.", p, strings.Join(sites, ", "))
	}
}

// An entrypoint on a datastore port is an open datastore port.
//
// Removing a router is not sufficient on its own. distribute-compose.yml configured Traefik with
// `--entrypoints.postgres.address=:5432` and `--entrypoints.redis.address=:6379`, which is what
// actually made the proxy listen; the routers merely said where to forward. A listener with no router
// behind it still answers, still terminates TLS, and still tells a scanner something is there. And the
// next person who adds a router finds the port already open and nothing objecting.
func TestNoEntrypointListensOnADatastorePort(t *testing.T) {
	// Ports a reverse proxy legitimately listens on.
	allowed := map[string]bool{
		"80": true, "443": true, // http, https
		"8083": true, "8084": true, // MQTT over WebSocket, ws and wss
		"3478": true, "5349": true, // STUN and TURN, needed for WebRTC through a NAT
	}

	for _, path := range deployedComposeFiles {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			m := traefikEntrypoint.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			name, port := m[1], m[2]
			if allowed[port] {
				continue
			}
			why, isManagement := managementPorts[port]
			if !isManagement {
				t.Errorf("%s:%d declares Traefik entrypoint %q on port %s, which is neither a known "+
					"proxy port nor a classified management port. Add it to the allowed list here with "+
					"a reason, or do not listen on it.", shortName(path), i+1, name, port)
				continue
			}
			t.Errorf("%s:%d makes Traefik listen on port %s for entrypoint %q — %s. Removing the "+
				"router is not enough; the entrypoint is the open socket.",
				shortName(path), i+1, port, name, why)
		}
	}
}

// A published port is the socket. No deployed stack may publish a datastore or console port.
//
// THIS IS THE THIRD LEVEL OF THE SAME EXPOSURE, and each level looked like the whole thing.
//
//  1. Postgres and Redis had Traefik TCP ROUTERS. Removed in e023656.
//  2. Traefik declared ENTRYPOINTS on :5432 and :6379, which is what made it listen. A listener with
//     no router still answers, so those were removed too, and TestNoEntrypointListensOnADatastorePort
//     was written on the belief that this closed it.
//  3. The traefik service still had `ports: - 5432:5432` and `- 6379:6379`. A port mapping opens the
//     port on the host with or without a router or an entrypoint. So the database and the cache were
//     still on the public interface of every stack that ships to customers.
//
// Level three was found by RUNNING the customer install, not by reading the file: the stack refused to
// start locally with "Bind for 0.0.0.0:6379 failed: port is already allocated". Nothing about the two
// earlier guards would have caught it, because they were both looking at Traefik's configuration rather
// than at Docker's.
//
// Loopback binds are fine and are the intended escape hatch: `127.0.0.1:5432:5432` reaches the database
// from the host without reaching it from anywhere else, which is what an operator running a migration
// actually needs.
func TestNoPublishedPortExposesADatastore(t *testing.T) {
	portsKey := regexp.MustCompile(`^\s{4}ports:\s*$`)
	// - 5432:5432 | - "5432:5432" | - 127.0.0.1:5432:5432 | - 50000-50100:50000-50100/udp
	mapping := regexp.MustCompile(
		`^\s+-\s+"?(?:([0-9.]+|\[[0-9a-fA-F:]+\]):)?([0-9]+(?:-[0-9]+)?):([0-9]+(?:-[0-9]+)?)(?:/(?:tcp|udp))?"?\s*$`)

	// Ports a host legitimately publishes. Everything else that is a known management or datastore port
	// is refused; anything unclassified is caught by TestEveryPublishedPortIsClassified's sibling below.
	publishable := map[string]string{
		"80": "http, redirected to https", "443": "https",
		"8083": "MQTT over WebSocket", "8084": "MQTT over WebSocket, TLS",
		"3478": "STUN/TURN", "5349": "TURN over TLS",
		"7880": "LiveKit signalling", "7881": "LiveKit TCP media fallback",
		// WebRTC media. A relay only works if the peer needing relaying can reach it, so this range is
		// public by definition rather than by convenience — the same reason 5349 is.
		"50000": "LiveKit UDP media range 50000-50100",
	}

	for _, path := range deployedComposeFiles {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}

		service, inServices, inPorts := "", false, false
		for i, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "services:") {
				inServices = true
				continue
			}
			if inServices && consoleGuardTopKey.MatchString(line) {
				inServices = false
			}
			if !inServices {
				continue
			}
			if m := consoleGuardService.FindStringSubmatch(line); m != nil {
				service, inPorts = m[1], false
				continue
			}
			if portsKey.MatchString(line) {
				inPorts = true
				continue
			}
			if !inPorts {
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			m := mapping.FindStringSubmatch(line)
			if m == nil {
				// A non-mapping line ends the list. Blank lines inside it are tolerated.
				if strings.TrimSpace(line) != "" {
					inPorts = false
				}
				continue
			}

			bind, hostPort := m[1], m[2]
			// A loopback bind is reachable from the host only, which is the point of allowing it.
			if bind == "127.0.0.1" || bind == "::1" || bind == "[::1]" {
				continue
			}
			// Ranges are media ports; the classification below is per single port.
			base := strings.SplitN(hostPort, "-", 2)[0]
			if _, ok := publishable[base]; ok {
				continue
			}
			why, isManagement := managementPorts[base]
			if !isManagement {
				t.Errorf("%s:%d publishes port %s from %s on all interfaces, and that port is "+
					"classified neither as publishable nor as a datastore/console. Add it to "+
					"`publishable` with the reason, bind it to 127.0.0.1, or do not publish it.",
					shortName(path), i+1, hostPort, service)
				continue
			}
			t.Errorf("%s:%d publishes %s on all interfaces from %s — %s. A published port is the "+
				"socket: it answers with or without a Traefik router or entrypoint in front of it. "+
				"Bind it to 127.0.0.1 if the host needs it.",
				shortName(path), i+1, hostPort, service, why)
		}
	}
}

// Dgraph's admin whitelist may not include the whole internet.
//
// This is separate from the routers because it is a second, independent lock on the same door, and it
// was also open. `--security whitelist` governs /alter, /admin, export and drop_all. It was
// 0.0.0.0/0 — every address there is — so even a future router added by accident would hand an
// anonymous caller the ability to drop the graph. Private ranges keep it reachable from inside any
// Docker network and from nowhere else.
func TestDgraphAdminWhitelistExcludesTheInternet(t *testing.T) {
	for _, path := range deployedComposeFiles {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if !strings.Contains(line, "--security whitelist=") {
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			value := line[strings.Index(line, "--security whitelist=")+len("--security whitelist="):]
			if f := strings.Fields(value); len(f) > 0 {
				value = f[0]
			}
			for _, cidr := range strings.Split(value, ",") {
				cidr = strings.TrimSpace(cidr)
				if cidr == "0.0.0.0/0" || cidr == "::/0" {
					t.Errorf("%s:%d whitelists %s for Dgraph admin operations, which is the entire "+
						"internet. That flag governs /alter, /admin, export and drop_all.",
						shortName(path), i+1, cidr)
				}
			}
		}
	}
}
