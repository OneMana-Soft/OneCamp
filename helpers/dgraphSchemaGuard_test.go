package helpers

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// No @upsert predicate carries a token index.
//
// WHY. Dgraph makes every key of an @upsert predicate's indexes a conflict
// key, so two transactions that write values sharing a token abort one
// another. A trigram index on user_name (@upsert) made each trigram such a
// key: ten people joining at once as crowd0..crowd9 share "cro", "row" and
// "owd", and their sign-ups aborted each other (18 Oct 2026, c925fc70;
// tests/integration TestTeammatesLandInTheDefaultChannels). An exact or hash
// index has one key per value, which is what @upsert is for. A search index
// (trigram, term, fulltext, ngram) belongs on a predicate of its own without
// @upsert, as user_search_name is. This reads the schema
// (initializers/dgraphInit/connectDgraphDB.go). It baselines nothing.
func TestNoUpsertPredicateCarriesATokenIndex(t *testing.T) {
	schema, err := os.ReadFile("../initializers/dgraphInit/connectDgraphDB.go")
	if err != nil {
		t.Fatal(err)
	}
	hits, upserts := tokenIndexesOnUpserts(string(schema))
	if upserts < 15 {
		t.Fatalf("read %d @upsert predicates from the schema; its shape changed", upserts)
	}
	for _, h := range hits {
		t.Errorf("%s: an @upsert predicate with a token index makes each token a conflict key, so concurrent writes of like values abort each other; index it exact or hash, and search a predicate of its own without @upsert", h)
	}
}

// The guard itself.
func TestTokenIndexesOnUpsertsFindsThem(t *testing.T) {
	schema := `
		user_uuid: string @index(exact) @upsert .
		user_name: string @index(exact, trigram) @upsert .
		user_full_name: string @index(trigram) .
		# note: @upsert with @index(term) in a comment is not a predicate
		dm_grouping_id: string @index(exact, term) .
		x_body: string @upsert @index(fulltext) .
		x_code: string @index(hash) @upsert .
		x_part: string @index( ngram ) @upsert .
	`
	hits, upserts := tokenIndexesOnUpserts(schema)
	if upserts != 5 {
		t.Errorf("counted %d @upsert predicates, want 5", upserts)
	}
	if strings.Join(hits, ",") != "user_name (trigram),x_body (fulltext),x_part (ngram)" {
		t.Errorf("found %v", hits)
	}
}

var schemaLine = regexp.MustCompile(`(?m)^\s*([a-z][a-z0-9_.]*)\s*:\s*\[?[a-zA-Z]+\]?([^#\n]*)\.\s*$`)
var indexArgs = regexp.MustCompile(`@index\(([^)]*)\)`)

// tokenTokenizers are the index tokenizers that split a value into many keys.
var tokenTokenizers = map[string]bool{"trigram": true, "term": true, "fulltext": true, "ngram": true}

// tokenIndexesOnUpserts is each @upsert predicate in schema with a token
// index, as "name (tokenizer)", and how many @upsert predicates there are.
func tokenIndexesOnUpserts(schema string) (hits []string, upserts int) {
	for _, m := range schemaLine.FindAllStringSubmatch(schema, -1) {
		directives := m[2]
		if !strings.Contains(directives, "@upsert") {
			continue
		}
		upserts++
		for _, ix := range indexArgs.FindAllStringSubmatch(directives, -1) {
			for _, tok := range strings.Split(ix[1], ",") {
				if tok = strings.TrimSpace(tok); tokenTokenizers[tok] {
					hits = append(hits, m[1]+" ("+tok+")")
				}
			}
		}
	}
	return hits, upserts
}
