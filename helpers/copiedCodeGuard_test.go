package helpers

import (
	"fmt"
	"go/scanner"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// No new copied code in production Go (owner, 22 Oct 2026: "uses super generic
// functions"; design/LEAD-RULES-2026-10-21.md, "Generic first": extend the
// helper that does the job, never write a second copy).
//
// Copied code is a run of copiedCodeTokens tokens found in two places: in two
// files, or twice in one file without overlapping. Comments, layout and import
// blocks are left out; names and literals are kept, so it finds code pasted
// and left as it was, and never two pieces that merely share a shape. Each
// file's count of tokens inside such runs is held to
// testdata/copied-code-baseline.txt, one "path count" line each: a new copy
// fails, naming where it was copied from, and lifting one out lowers the count
// (WRITE_COPIED_CODE_BASELINE=1 rewrites the file, for the change that does).
//
// 100 tokens is ten to fifteen lines of Go: past an if err != nil, a struct
// literal or a log line, and short of most functions.
const (
	copiedCodeTokens   = 100
	copiedCodeBaseline = "testdata/copied-code-baseline.txt"
	copiedCodeHeader   = `# Copied code in production Go: per file, how many tokens sit in a run of 100 that is
# also somewhere else (helpers/copiedCodeGuard_test.go). A line is debt: lift the copy into
# one shared function and lower the line (WRITE_COPIED_CODE_BASELINE=1 rewrites this file).
`
)

func TestNoNewCopiedCodeInProductionGo(t *testing.T) {
	files := productionGoFiles(t)
	if len(files) < 1000 {
		t.Fatalf("scanned %d files: the check is no longer reading the server's code", len(files))
	}
	sources := make([]copySource, 0, len(files))
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, copySource{path: strings.TrimPrefix(path, "../"), toks: goCopyTokens(src)})
	}
	found, from := copiedRuns(sources, copiedCodeTokens)
	advice := "Lift the copy into one shared function, and call it from both places (LEAD-RULES, \"Generic first\"). " +
		"Where each was copied from:\n  " + strings.Join(copyExamples(from, found, copiedCodeBaseline), "\n  ")
	checkRatchet(t, found, copiedCodeBaseline, copiedCodeHeader, "WRITE_COPIED_CODE_BASELINE", advice)
}

// copySource is a file's tokens, each with its line.
type copySource struct {
	path string
	toks []copyToken
}

type copyToken struct {
	text string
	line int
}

// goCopyTokens is a Go file's tokens without comments, the newlines' implied
// semicolons and its import declarations.
func goCopyTokens(src []byte) []copyToken {
	fset := token.NewFileSet()
	f := fset.AddFile("", -1, len(src))
	var s scanner.Scanner
	s.Init(f, src, nil, 0)
	var out []copyToken
	importing, depth := false, 0
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			return out
		}
		if tok == token.IMPORT {
			importing = true
			continue
		}
		if importing {
			switch {
			case tok == token.LPAREN:
				depth++
			case tok == token.RPAREN:
				depth--
				importing = depth > 0
			case depth == 0 && tok == token.SEMICOLON:
				importing = false
			}
			continue
		}
		if tok == token.SEMICOLON && lit == "\n" {
			continue
		}
		text := lit
		if text == "" {
			text = tok.String()
		}
		out = append(out, copyToken{text, f.Line(pos)})
	}
}

// copyAt is where a run starts: a source and a token index.
type copyAt struct{ src, at int }

// copiedRuns counts, per path, the tokens inside a run of n tokens that is
// also somewhere else, and for each such path the first place a run of it was
// copied from (for the failure's message). Runs are found by a rolling hash
// over token ids, so it reads the whole server in about a second.
func copiedRuns(sources []copySource, n int) (map[string]int, map[string][2]string) {
	ids := map[string]uint64{}
	idOf := func(s string) uint64 {
		id, ok := ids[s]
		if !ok {
			id = uint64(len(ids) + 1)
			ids[s] = id
		}
		return id
	}
	const base = 1_000_003
	var top uint64 = 1 // base^(n-1)
	for i := 1; i < n; i++ {
		top *= base
	}
	hashes := make([][]uint64, len(sources))
	seen := map[uint64]int{}
	for si, s := range sources {
		if len(s.toks) < n {
			continue
		}
		hs := make([]uint64, 0, len(s.toks)-n+1)
		var h uint64
		for i, tk := range s.toks {
			if i >= n {
				h -= idOf(s.toks[i-n].text) * top
			}
			h = h*base + idOf(tk.text)
			if i >= n-1 {
				hs = append(hs, h)
				seen[h]++
			}
		}
		hashes[si] = hs
	}
	where := map[uint64][]copyAt{}
	for si, hs := range hashes {
		for i, h := range hs {
			if seen[h] > 1 {
				where[h] = append(where[h], copyAt{si, i})
			}
		}
	}
	covered := make([][]bool, len(sources))
	from := map[string][2]string{}
	for _, at := range where {
		// Runs repeated only inside one stretch (a long run of one token) aren't copies.
		distinct := at[:1]
		for _, a := range at[1:] {
			last := distinct[len(distinct)-1]
			if a.src != last.src || a.at-last.at >= n {
				distinct = append(distinct, a)
			}
		}
		if len(distinct) < 2 {
			continue
		}
		for k, a := range distinct {
			if covered[a.src] == nil {
				covered[a.src] = make([]bool, len(sources[a.src].toks))
			}
			for j := a.at; j < a.at+n; j++ {
				covered[a.src][j] = true
			}
			path := sources[a.src].path
			if _, named := from[path]; !named {
				other := distinct[(k+1)%len(distinct)]
				from[path] = [2]string{
					fmt.Sprintf("%s:%d", path, sources[a.src].toks[a.at].line),
					fmt.Sprintf("%s:%d", sources[other.src].path, sources[other.src].toks[other.at].line),
				}
			}
		}
	}
	found := map[string]int{}
	for si, c := range covered {
		for _, on := range c {
			if on {
				found[sources[si].path]++
			}
		}
	}
	return found, from
}

// copyExamples names, for each path over its baseline line, one run and where
// else it is.
func copyExamples(from map[string][2]string, found map[string]int, baselinePath string) []string {
	baseline, _ := readRatchetBaseline(baselinePath)
	var out []string
	for path, n := range found {
		if allowed, listed := baseline[path]; listed && n <= allowed {
			continue
		}
		if pair, ok := from[path]; ok {
			out = append(out, pair[0]+" is also at "+pair[1])
		}
	}
	sort.Strings(out)
	return out
}

// The detector, on the cases that shaped it.
func TestCopiedRunsFindPastedCodeOnly(t *testing.T) {
	body := "func a(x int) int {\n" + strings.Repeat("\tx = x*3 + 7\n", 20) + "\treturn x\n}\n"
	renamed := strings.ReplaceAll(body, "x", "y")
	srcs := func(files ...string) []copySource {
		out := make([]copySource, len(files))
		for i, f := range files {
			out[i] = copySource{path: fmt.Sprintf("f%d.go", i), toks: goCopyTokens([]byte("package p\nimport \"fmt\"\n" + f))}
		}
		return out
	}
	found, _ := copiedRuns(srcs(body, body), 100)
	if found["f0.go"] == 0 || found["f1.go"] == 0 {
		t.Errorf("a function pasted into another file isn't found: %v", found)
	}
	if found, _ := copiedRuns(srcs(body, renamed), 100); len(found) != 0 {
		t.Errorf("code of the same shape with other names counts as a copy: %v", found)
	}
	if found, _ := copiedRuns(srcs(body+strings.ReplaceAll(body, "func a", "func b")), 100); found["f0.go"] == 0 {
		t.Errorf("a function pasted twice into one file isn't found: %v", found)
	}
	short := "func a() { fmt.Println(1) }\n"
	if found, _ := copiedRuns(srcs(short, short), 100); len(found) != 0 {
		t.Errorf("a few lines alike count as a copy: %v", found)
	}
}
