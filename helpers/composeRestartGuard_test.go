package helpers

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every long-running service in a deployed compose file must come back after a host reboot.
//
// WHY THIS IS A TEST. Docker starts a container on daemon start only for `always` and
// `unless-stopped`. A missing policy means `no`, and the docs are explicit that `on-failure` "doesn't
// restart the container if the daemon restarts". Neither of those is visible as a problem: the stack
// runs perfectly for months, and the fault only appears on the one occasion nobody is watching, which
// is a kernel update at 3am.
//
// It had already happened once. collaboration-service had no policy, one unhandled rejection killed
// it, and every doc and board showed "Reconnecting to collaboration server…" for two weeks while the
// rest of the product worked — because the client was correctly reconnecting to something that was not
// there. That was fixed in b91fc42, as a single service.
//
// Auditing the rest found EIGHT more in final-compose.yml alone, including Postgres, both Dgraph nodes,
// the MQTT broker and object storage — while go-service has `always`. So a reboot brought the stack
// back as a shell: the API restarted immediately into a world with no databases. The same fault was in
// distribute-compose.yml too, which is what ships to customers, so it was not one host's
// misconfiguration but the default every deployment inherited.
//
// A policy is one line and its absence is invisible in review. That is exactly what a test is for.

// rebootSurviving are the only two policies Docker acts on when the daemon starts.
var rebootSurviving = map[string]bool{"always": true, "unless-stopped": true}

// deployedComposeFiles are the files that describe a real running stack. The per-component compose
// files (postgres-compose.yml and friends) are deliberately NOT here: several were last touched in
// 2025, they overlap with these, and treating them as deployment descriptions would assert something
// this project does not actually believe about them.
//
// shared-compose.yml and customer-compose.yml both used to be here, correctly: they were as deployed
// as the rest. Both have been deleted along with the multi-tenant-on-one-host topology they described,
// so their entries went with them. Nothing ran customer-compose.yml: the zip ships
// distribute-compose.yml as sample-compose.yml, and cloud provisioning runs that same file with
// --project-name onecamp, one workspace per box. Removing a file from this list is only ever right
// when the file is gone — a stack that still runs and is merely out of fashion belongs here, because
// the reason this list exists is that the last omission was invisible for months.
var deployedComposeFiles = []string{
	"../final-compose.yml",
	"../distribute-compose.yml",
}

// oneShotServices legitimately must not restart. An init job that has finished is finished; restarting
// it forever is not resilience, it is a loop.
var oneShotServices = map[string]string{
	"ollama-init": "one-shot model pull; `no` is correct and restarting a completed job would loop",
}

var (
	// A service key. The trailing comment is NOT optional decoration in this pattern — leaving it out
	// is what made the first version of this guard lie. See TestComposeParserHasNoBlindSpots.
	composeServiceLine = regexp.MustCompile(`^  ([a-z0-9][a-z0-9._-]*):\s*(?:#.*)?$`)
	composeRestartLine = regexp.MustCompile(`^\s+restart:\s*"?([a-z-]+)"?\s*$`)
	composeTopLevelKey = regexp.MustCompile(`^[a-zA-Z]`)

	// Deliberately sloppy: anything at service indentation that ends in a colon. Used only to find what
	// the strict pattern above fails to see.
	composeServiceLineLoose = regexp.MustCompile(`^  [^\s#][^:]*:`)
)

// restartPolicies returns service name -> policy ("" when none is declared) for one compose file.
func restartPolicies(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v. If a compose file was renamed, update deployedComposeFiles — "+
			"this check is worth nothing if it silently scans nothing", path, err)
	}

	out := map[string]string{}
	inServices := false
	current := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "services:") {
			inServices = true
			continue
		}
		// networks:, volumes: and friends end the block.
		if inServices && composeTopLevelKey.MatchString(line) {
			inServices = false
		}
		if !inServices {
			continue
		}
		if m := composeServiceLine.FindStringSubmatch(line); m != nil {
			current = m[1]
			out[current] = ""
			continue
		}
		if current == "" {
			continue
		}
		if m := composeRestartLine.FindStringSubmatch(line); m != nil && out[current] == "" {
			out[current] = m[1]
		}
	}

	if len(out) < 5 {
		t.Fatalf("%s parsed to only %d services; the parser no longer fits the file", path, len(out))
	}
	return out
}

func TestDeployedServicesSurviveAReboot(t *testing.T) {
	for _, path := range deployedComposeFiles {
		t.Run(strings.TrimPrefix(path, "../"), func(t *testing.T) {
			policies := restartPolicies(t, path)

			var offenders []string
			for svc, policy := range policies {
				if _, ok := oneShotServices[svc]; ok {
					continue
				}
				if rebootSurviving[policy] {
					continue
				}
				shown := policy
				if shown == "" {
					shown = "no policy declared"
				}
				offenders = append(offenders, fmt.Sprintf("%s (%s)", svc, shown))
			}
			sort.Strings(offenders)

			if len(offenders) > 0 {
				t.Errorf("%d service(s) would NOT be started when the Docker daemon restarts, so a host "+
					"reboot leaves them down until somebody notices. Use `unless-stopped` unless the "+
					"service is a one-shot job, in which case add it to oneShotServices with the "+
					"reason:\n  %s", len(offenders), strings.Join(offenders, "\n  "))
			}
		})
	}
}

// A service the parser cannot see is a service this guard silently exempts.
//
// WHY THIS EXISTS, AND IT IS THIS GUARD'S OWN BUG. The first version of composeServiceLine ended in
// `:\s*$`, which does not match a service declared with a trailing comment:
//
//	opensearch-node1: # This is also the hostname of the container within the Docker network
//
// distribute-compose.yml -- the stack that ships to customers -- declares the search node exactly that
// way, and it had NO restart policy. So the guard reported all three deployed files clean while never
// having looked at one of the services in the one that matters most, and the commit that added it
// claimed the fault was closed. A parser bug in a test does not fail; it under-reports, which is worse
// than no test, because it also stops anyone looking again.
//
// The fix is not just the regex. Any future shape the strict pattern does not handle -- a quoted key, a
// different indent, a name with a character the class omits -- would fail the same silent way. So this
// compares what the parser sees against a deliberately sloppy pattern and fails on the difference. The
// parser is allowed to be strict; it is not allowed to be strict and quiet.
func TestComposeParserHasNoBlindSpots(t *testing.T) {
	for _, path := range deployedComposeFiles {
		t.Run(strings.TrimPrefix(path, "../"), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("cannot read %s: %v", path, err)
			}

			var unseen []string
			inServices := false
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
				// Only lines at service indentation, and only ones the sloppy pattern calls a key.
				if !composeServiceLineLoose.MatchString(line) {
					continue
				}
				if composeServiceLine.MatchString(line) {
					continue
				}
				unseen = append(unseen, fmt.Sprintf("%d: %s", i+1, strings.TrimSpace(line)))
			}

			if len(unseen) > 0 {
				t.Errorf("%d line(s) look like a service declaration but the parser does not match "+
					"them, so those services are silently exempt from every check in this file:\n  %s\n"+
					"Widen composeServiceLine rather than assuming the file is wrong.",
					len(unseen), strings.Join(unseen, "\n  "))
			}
		})
	}
}

// The exemption list may not rot into a place where things are parked.
func TestOneShotExemptionsStillExistAndStillOptOut(t *testing.T) {
	seen := map[string]string{}
	for _, path := range deployedComposeFiles {
		for svc, policy := range restartPolicies(t, path) {
			if _, ok := oneShotServices[svc]; ok {
				seen[svc] = policy
			}
		}
	}

	for svc, reason := range oneShotServices {
		policy, present := seen[svc]
		if !present {
			t.Errorf("%q is exempted as a one-shot service but no deployed compose file declares it. "+
				"Remove the exemption: a list of names that no longer exist stops describing anything "+
				"(reason on file: %s)", svc, reason)
			continue
		}
		// If somebody gives it a restart policy after all, the exemption is the wrong answer and should
		// go rather than sit there contradicting the file.
		if rebootSurviving[policy] {
			t.Errorf("%q now has restart: %s, so it is no longer a one-shot service. Remove it from "+
				"oneShotServices.", svc, policy)
		}
	}
}
