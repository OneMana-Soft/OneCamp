package linear

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Every query the import sends is checked against Linear's public schema,
// recorded in testdata/schema-fields.json (from github.com/linear/linear,
// packages/sdk/src/schema.graphql; regenerate it with
// testdata/extract_schema.py, naming the types the queries reach). A field
// Linear has deprecated, or a query over Linear's complexity cap, fails
// here, before Linear retires the field and every import stops in silence,
// as Jira's /search and Todoist's Sync v9 did (22 Oct 2026).
func TestEveryQueryReadsLiveFieldsUnderTheCap(t *testing.T) {
	raw, err := os.ReadFile("testdata/schema-fields.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Types map[string]map[string]struct {
			Type       string `json:"type"`
			Deprecated bool   `json:"deprecated"`
		} `json:"types"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	queries := map[string]string{
		"viewer": viewerQuery, "discover": discoverQuery, "users": usersQuery,
		"teams": teamsQuery, "projects": projectsQuery, "issues": teamIssuesQuery,
		"issue comments": issueCommentsQuery, "issue files": issueAttachmentsQuery,
		"issue relations": issueRelationsQuery,
	}
	for name, q := range queries {
		sel, err := parseSelection(q)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		var problems []string
		var cost func(typ string, fields []gqlField, path string) float64
		cost = func(typ string, fields []gqlField, path string) float64 {
			known, ok := schema.Types[typ]
			if !ok {
				problems = append(problems, fmt.Sprintf("%s: type %s isn't recorded (add it to testdata)", path, typ))
				return 0
			}
			sum := 0.0
			for _, f := range fields {
				at := path + "." + f.name
				def, ok := known[f.name]
				switch {
				case !ok:
					problems = append(problems, at+": no such field on "+typ)
					continue
				case def.Deprecated:
					problems = append(problems, at+": deprecated by Linear")
				}
				switch {
				case len(f.children) == 0:
					sum += 0.1 // a property
				case strings.HasSuffix(def.Type, "Connection"):
					// A connection multiplies what it holds by its page size
					// (first), or 50 when none is given.
					n := 50.0
					if f.first > 0 {
						n = float64(f.first)
					}
					sum++
					for _, c := range f.children {
						inner, ok := schema.Types[def.Type][c.name]
						if !ok {
							problems = append(problems, at+"."+c.name+": no such field on "+def.Type)
							continue
						}
						if c.name == "nodes" {
							sum += n * (1 + cost(inner.Type, c.children, at+".nodes"))
						} else {
							sum += 1 + cost(inner.Type, c.children, at+"."+c.name)
						}
					}
				default:
					sum += 1 + cost(def.Type, f.children, at) // an object
				}
			}
			return sum
		}
		total := cost("Query", sel, name)
		for _, p := range problems {
			t.Error(p)
		}
		// Linear rejects any query above 10,000 points, always.
		if total > 10000 {
			t.Errorf("%s: about %.0f points, over Linear's cap of 10,000 (a page would always be refused)", name, total)
		}
	}
}

// gqlField is one field of a selection set: its name, its page size when it
// says `first: N`, and what it selects.
type gqlField struct {
	name     string
	first    int
	children []gqlField
}

// parseSelection reads a query's top selection set: enough GraphQL for the
// import's queries (fields, arguments, nested sets; no fragments). Pure.
func parseSelection(q string) ([]gqlField, error) {
	toks := tokenize(q)
	i := 0
	for i < len(toks) && toks[i] != "{" {
		if toks[i] == "(" {
			i = skipParens(toks, i)
			continue
		}
		i++
	}
	if i == len(toks) {
		return nil, fmt.Errorf("no selection set")
	}
	fields, next, err := parseSet(toks, i)
	if err == nil && next != len(toks) {
		err = fmt.Errorf("text after the selection set")
	}
	return fields, err
}

func parseSet(toks []string, i int) ([]gqlField, int, error) {
	if toks[i] != "{" {
		return nil, i, fmt.Errorf("want {, got %q", toks[i])
	}
	i++
	var out []gqlField
	for i < len(toks) && toks[i] != "}" {
		f := gqlField{name: toks[i]}
		i++
		if i < len(toks) && toks[i] == "(" {
			end := skipParens(toks, i)
			for j := i; j < end-2; j++ {
				if toks[j] == "first" && toks[j+1] == ":" {
					f.first, _ = strconv.Atoi(toks[j+2])
				}
			}
			i = end
		}
		if i < len(toks) && toks[i] == "{" {
			children, next, err := parseSet(toks, i)
			if err != nil {
				return nil, next, err
			}
			f.children, i = children, next
		}
		out = append(out, f)
	}
	if i == len(toks) {
		return nil, i, fmt.Errorf("unclosed {")
	}
	return out, i + 1, nil
}

func skipParens(toks []string, i int) int {
	depth := 0
	for ; i < len(toks); i++ {
		switch toks[i] {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return i
}

func tokenize(q string) []string {
	var out []string
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == ' ' || c == '\n' || c == '\t' || c == '\r' || c == ',':
			i++
		case strings.ContainsRune("{}():$!", rune(c)):
			out = append(out, string(c))
			i++
		case c == '"':
			j := i + 1
			for j < len(q) && q[j] != '"' {
				j++
			}
			out = append(out, q[i:j+1])
			i = j + 1
		default:
			j := i
			for j < len(q) && !strings.ContainsRune(" \n\t\r,{}():$!\"", rune(q[j])) {
				j++
			}
			out = append(out, q[i:j])
			i = j
		}
	}
	return out
}
