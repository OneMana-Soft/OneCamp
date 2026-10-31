package providers_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	_ "github.com/akashc777/OneCamp/business/Import/providers"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// The token table's provider CHECK admits every registered provider (22 Oct
// 2026). Each live-API provider's migration rewrites the one CHECK whole
// (monday.com's 187, then Basecamp's, Airtable's and Miro's, written side by
// side on their own branches), so each lists only the providers its branch
// knew, and whichever lands last would refuse the others' tokens: a person
// could no longer connect a provider that had worked the day before. The
// migration that sets the CHECK last must name every provider registered here
// that reads a live API. One read from an uploaded file (a CSV of tasks) keeps
// no token, so the CHECK has nothing to admit for it.
func TestTheTokenCheckAdmitsEveryProvider(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	check := regexp.MustCompile(`(?is)ADD\s+CONSTRAINT\s+import_oauth_tokens_provider_check\s+CHECK\s*\(\s*provider\s+IN\s*\(([^)]*)\)`)
	last, lastFile, list := -1, "", ""
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		v, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil || v < last {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if m := check.FindAllStringSubmatch(string(body), -1); len(m) > 0 {
			last, lastFile, list = v, name, m[len(m)-1][1]
		}
	}
	if last < 0 {
		t.Fatal("no migration sets import_oauth_tokens_provider_check")
	}
	admitted := map[string]bool{}
	for _, p := range strings.Split(list, ",") {
		admitted[strings.Trim(strings.TrimSpace(p), "'")] = true
	}
	names := importProvider.Names()
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("no provider registered: the providers package's imports didn't run")
	}
	for _, n := range names {
		if !slices.Contains(importProvider.Get(n).SupportedSources(), importModels.SourceAPI) {
			continue
		}
		if !admitted[n] {
			t.Errorf("%s, the last migration to set the token CHECK, refuses %q's tokens", lastFile, n)
		}
	}
}
