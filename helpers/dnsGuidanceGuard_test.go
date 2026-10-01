package helpers

// Every hostname the stack publishes must appear in the DNS list the installer prints.
//
// THIS DRIFTED ONCE AND COST VIDEO CALLS. LiveKit and the collaboration service were
// added to distribute-compose.yml with Traefik router rules of their own. The
// published DNS guide had been trimmed earlier, correctly at the time, on the
// grounds that "those services are not in the shipped stack at all" — and nothing
// re-checked that claim when they arrived. So the product shipped a call button, the
// documentation told customers not to create the record it needed, and Let's Encrypt
// therefore issued no certificate for it.
//
// The failure is invisible in review: both files look right on their own. Only the
// relationship between them is wrong, so the relationship is what gets pinned.
//
// A hostname that is deliberately NOT published needs no record, and Traefik's own
// dashboard is the one case: it is routed so that basic auth can protect it, and
// reaching it is a tunnel rather than a name.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// unpublishedHosts are routed but deliberately have no DNS record, with the reason.
var unpublishedHosts = map[string]string{
	"TRAEFIK_DOMAIN": "the dashboard is behind basic auth and reached by tunnel, not by name",
}

var (
	composeHostRule = regexp.MustCompile(`Host\(` + "`" + `\$\{([A-Za-z_][A-Za-z0-9_]*)[^}]*\}` + "`" + `\)`)
	// Anchored to the end of the line and stripped of the ONE trailing paren that
	// closes the call. A lazy [^)]* stops at the first ")", which for a value like
	// onecamp-livekit.$(DOMAIN) captures "onecamp-livekit.$(DOMAIN" — still a
	// substring of the real thing, so the check would pass on a truncated name and
	// nobody would know it had stopped being exact.
	setEnvCall = regexp.MustCompile(`(?m)^\s*\$\(call set_env,([A-Za-z_][A-Za-z0-9_]*),(.*)\)\s*$`)
)

func TestPublishedHostsAreInTheInstallersDNSList(t *testing.T) {
	compose, err := os.ReadFile("../distribute-compose.yml")
	if err != nil {
		t.Fatalf("reading the shipped compose file: %v", err)
	}
	makefile, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatalf("reading the installer makefile: %v", err)
	}

	// What replace-domain assigns to each variable, which is what a customer's DNS
	// record has to be named.
	assigned := map[string]string{}
	for _, m := range setEnvCall.FindAllStringSubmatch(string(makefile), -1) {
		if _, seen := assigned[m[1]]; !seen {
			assigned[m[1]] = strings.TrimSpace(m[2])
		}
	}

	// The block replace-domain prints. Bounded so this checks the guidance rather
	// than merely finding the name somewhere else in a 1500-line file.
	printed := dnsGuidanceBlock(t, string(makefile))

	vars := map[string]bool{}
	for _, m := range composeHostRule.FindAllStringSubmatch(string(compose), -1) {
		vars[m[1]] = true
	}
	if len(vars) < 3 {
		t.Fatalf("found only %d routed hostnames, so this check stopped being able to fail", len(vars))
	}

	for v := range vars {
		if why, ok := unpublishedHosts[v]; ok {
			if strings.Contains(printed, assigned[v]) && assigned[v] != "" {
				t.Errorf("%s is printed as a DNS record to create, but %s", v, why)
			}
			continue
		}
		host, ok := assigned[v]
		if !ok {
			t.Errorf("the compose file routes ${%s} but replace-domain never sets it, so a\n"+
				"customer has no way to know what to call the record", v)
			continue
		}
		if !strings.Contains(printed, host) {
			t.Errorf("the compose file routes ${%s} (%s) but the installer never tells the\n"+
				"customer to create that record. Without it Let's Encrypt issues no\n"+
				"certificate and the feature behind it fails with nothing naming DNS.", v, host)
		}
	}
}

// The workspace hostname is an A record for this server ONLY when this server
// serves the web app (the "web" profile). Otherwise the web app is deployed
// elsewhere, and pointing the address the team opens at this machine is the
// one DNS mistake that makes a working install look broken.
func TestTheWebAppHostIsPresentedByWhereTheWebAppRuns(t *testing.T) {
	makefile, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatalf("reading the installer makefile: %v", err)
	}
	start := strings.Index(string(makefile), "\nreplace-domain:")
	if start < 0 {
		t.Fatal("replace-domain is gone; if the install moved, move this check with it")
	}
	body := string(makefile)[start:]

	// The conditional's own else and endif: replace-domain opens with an
	// ifndef DOMAIN block that has an endif of its own.
	on := strings.Index(body, "ifneq ($(WEB_PROFILE),)")
	els, end := -1, -1
	if on >= 0 {
		if i := strings.Index(body[on:], "\nelse\n"); i >= 0 {
			els = on + i
		}
		if i := strings.Index(body[on:], "\nendif\n"); i >= 0 {
			end = on + i
		}
	}
	if on < 0 || els < on || end < els {
		t.Fatal("the web app's DNS guidance is no longer split by whether this server serves it")
	}
	// Only what is PRINTED counts: the recipe also assigns the hostname.
	echoed := func(part string) string {
		var out []string
		for _, line := range strings.Split(part, "\n") {
			if t := strings.TrimSpace(line); strings.HasPrefix(t, "@echo") || strings.HasPrefix(t, "echo") {
				out = append(out, line)
			}
		}
		return strings.Join(out, "\n")
	}
	always := echoed(body[:on])
	served, elsewhere := echoed(body[on:els]), echoed(body[els:end])

	if at := strings.Index(strings.ToLower(always), "pointing at this server"); at < 0 {
		t.Fatal("the installer no longer says which records point at this server")
	}
	if strings.Contains(always, "onecamp.$(DOMAIN)") {
		t.Error("onecamp.$(DOMAIN) is listed unconditionally, whether or not this server serves the web app")
	}
	if !strings.Contains(served, "onecamp.$(DOMAIN)") || !strings.Contains(strings.ToLower(served), "served here") {
		t.Error("with the web profile on, onecamp.$(DOMAIN) must be listed as served by this server")
	}
	if !strings.Contains(elsewhere, "onecamp.$(DOMAIN)") || !strings.Contains(elsewhere, "WEB APP") {
		t.Error("with the web app hosted elsewhere, onecamp.$(DOMAIN) must point at that deployment")
	}
}

// dnsGuidanceBlock returns only the lines replace-domain ECHOES.
//
// Only the echoes, deliberately. The recipe also contains the set_env calls that
// assign each hostname, so a check run against the whole target finds every name in
// it and passes whether or not the customer is ever told about one. That is the
// check believing itself.
func dnsGuidanceBlock(t *testing.T, makefile string) string {
	t.Helper()
	start := strings.Index(makefile, "\nreplace-domain:")
	if start < 0 {
		t.Fatal("replace-domain is gone; if the install moved, move this check with it")
	}
	rest := makefile[start+1:]
	// A target ends at the next line that starts in column zero and is not part of
	// the recipe.
	for i, line := range strings.Split(rest, "\n") {
		if i == 0 || line == "" || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, " ") ||
			strings.HasPrefix(line, "#") || strings.HasPrefix(line, "ifndef") ||
			strings.HasPrefix(line, "endif") || strings.HasPrefix(line, "else") ||
			strings.HasPrefix(line, "ifneq") || strings.HasPrefix(line, "ifeq") {
			continue
		}
		rest = rest[:strings.Index(rest, "\n"+line)]
		break
	}
	var echoed []string
	for _, line := range strings.Split(rest, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "@echo") || strings.HasPrefix(t, "echo") {
			echoed = append(echoed, line)
		}
	}
	return strings.Join(echoed, "\n")
}
