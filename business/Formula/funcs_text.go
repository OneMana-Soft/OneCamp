package formula

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"
	"sync"
	"unicode/utf8"
)

// register adds a group of functions to the catalogue; a name taken twice is
// a mistake caught when the server starts.
func register(group map[string]*function) {
	for name, f := range group {
		if _, taken := functions[name]; taken {
			panic("formula: " + name + " is defined twice")
		}
		functions[name] = f
	}
}

func init() {
	register(map[string]*function{
		"SEARCH":               {min: 2, max: 3, result: KindNumber, eager: fnSearch},
		"REPLACE":              {min: 4, max: 4, result: KindText, eager: fnReplace},
		"T":                    {min: 1, max: 1, result: KindText, eager: fnT},
		"ENCODE_URL_COMPONENT": {min: 1, max: 1, result: KindText, eager: fnEncodeURL},
		"REGEX_MATCH":          {min: 2, max: 2, result: KindBool, eager: fnRegexMatch},
		"REGEX_EXTRACT":        {min: 2, max: 2, result: KindText, eager: fnRegexExtract},
		"REGEX_REPLACE":        {min: 3, max: 3, result: KindText, eager: fnRegexReplace},
	})
}

// SEARCH(what, in, [from]) is FIND that gives a blank rather than 0 when what
// isn't there, as Airtable's does.
func fnSearch(e *env, a []Value) Value {
	v := fnFind(e, a)
	if v.Kind == KindNumber && v.Num == 0 {
		return Blank()
	}
	return v
}

// REPLACE(text, start, count, with) puts with in place of count characters
// from start (counted from 1).
func fnReplace(e *env, a []Value) Value {
	s, with := a[0].Text(), a[3].Text()
	start, bad, ok := count("REPLACE", a[1])
	if !ok {
		return bad
	}
	if start < 1 {
		return Error("REPLACE counts from 1")
	}
	n, bad, ok := count("REPLACE", a[2])
	if !ok {
		return bad
	}
	i := after(e, s, start-1)
	j := i + after(e, s[i:], n)
	if len(s)-(j-i)+len(with) > 4*max(maxText, utf8.RuneCountInString(s)) {
		return tooLong()
	}
	return textResult(s[:i]+with+s[j:], max(maxText, utf8.RuneCountInString(s)))
}

// T(value) is the value when it's text, and a blank otherwise.
func fnT(_ *env, a []Value) Value {
	if a[0].Kind == KindText {
		return a[0]
	}
	return Blank()
}

// ENCODE_URL_COMPONENT(text) escapes text for a link, as JavaScript's
// encodeURIComponent does: letters, digits and -_.!~*'() are kept.
func fnEncodeURL(e *env, a []Value) Value {
	s := a[0].Text()
	e.spend(len(s))
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return textResult(b.String(), 3*max(maxText, len(s)))
}

// Regular expressions are Go's RE2, as Airtable's are: matching takes time in
// proportion to the text, never more, whatever the pattern.
const (
	// maxPattern is the longest pattern, in characters.
	maxPattern = 500
	// maxPatternInst is the most instructions a pattern compiles to:
	// "(a{1000}){1000}" is short and would be millions.
	maxPatternInst = 5000
	// patternCache is how many compiled patterns are kept between rows.
	patternCache = 256
)

type compiledPattern struct {
	re *regexp.Regexp
	// cost is the steps one match takes for each byte of text it looks
	// through (regexCost).
	cost int
	err  string
}

var patterns = struct {
	sync.Mutex
	m map[string]*compiledPattern
}{m: map[string]*compiledPattern{}}

// regexCost is the steps a pattern of inst instructions takes for each byte
// of text it looks through: a few for any pattern (even a short one with
// groups and alternatives goes at about 170 ns a byte), and more for a long
// one, which keeps a thread for each instruction (measured; held by
// TestNewFunctionsAreCharged).
func regexCost(inst int) int { return 4 + inst/32 }

// pattern compiles a regular expression for function name, or says why it
// can't be one. Compiling is charged by the pattern's length, cached or not,
// so a formula's cost doesn't depend on what ran before it.
func pattern(e *env, name string, v Value) (*compiledPattern, Value, bool) {
	src := v.Text()
	e.slow(len(src))
	if utf8.RuneCountInString(src) > maxPattern {
		return nil, Error(fmt.Sprintf("%s's pattern is longer than %d characters", name, maxPattern)), false
	}
	patterns.Lock()
	p, ok := patterns.m[src]
	patterns.Unlock()
	if !ok {
		p = compilePattern(src)
		patterns.Lock()
		if len(patterns.m) >= patternCache {
			patterns.m = map[string]*compiledPattern{}
		}
		patterns.m[src] = p
		patterns.Unlock()
	}
	if p.err != "" {
		return nil, Error(fmt.Sprintf("%s's pattern %s", name, p.err)), false
	}
	return p, Value{}, true
}

func compilePattern(src string) *compiledPattern {
	parsed, err := syntax.Parse(src, syntax.Perl)
	if err != nil {
		return &compiledPattern{err: "isn't a regular expression: " + strings.TrimPrefix(err.Error(), "error parsing regexp: ")}
	}
	prog, err := syntax.Compile(parsed.Simplify())
	if err != nil || len(prog.Inst) > maxPatternInst {
		return &compiledPattern{err: "is too complicated to work out"}
	}
	re, err := regexp.Compile(src)
	if err != nil {
		return &compiledPattern{err: "is too complicated to work out"}
	}
	return &compiledPattern{re: re, cost: regexCost(len(prog.Inst))}
}

// match charges for one pass of p over text.
func (p *compiledPattern) match(e *env, text string) {
	e.spend((len(text) + 1) * p.cost)
}

// REGEX_MATCH(text, pattern) is whether the pattern matches anywhere in text.
func fnRegexMatch(e *env, a []Value) Value {
	p, bad, ok := pattern(e, "REGEX_MATCH", a[1])
	if !ok {
		return bad
	}
	s := a[0].Text()
	p.match(e, s)
	return Bool(p.re.MatchString(s))
}

// REGEX_EXTRACT(text, pattern) is the first match: its first group when the
// pattern has one, otherwise the whole match; a blank when nothing matches.
func fnRegexExtract(e *env, a []Value) Value {
	p, bad, ok := pattern(e, "REGEX_EXTRACT", a[1])
	if !ok {
		return bad
	}
	s := a[0].Text()
	p.match(e, s)
	m := p.re.FindStringSubmatchIndex(s)
	if m == nil {
		return Blank()
	}
	if len(m) >= 4 && m[2] >= 0 {
		return Text(s[m[2]:m[3]])
	}
	return Text(s[m[0]:m[1]])
}

// REGEX_REPLACE(text, pattern, with) replaces every match; with can name the
// pattern's groups as $1 or ${name}. Its text is held to maxText characters,
// or the text's own length when that's longer, and is built a match at a
// time so a long with between many empty matches stops at the limit.
func fnRegexReplace(e *env, a []Value) Value {
	p, bad, ok := pattern(e, "REGEX_REPLACE", a[1])
	if !ok {
		return bad
	}
	s, with := a[0].Text(), a[2].Text()
	p.match(e, s)
	limit := max(maxText, utf8.RuneCountInString(s))
	var out []byte
	last := 0
	for _, m := range p.re.FindAllStringSubmatchIndex(s, -1) {
		out = append(out, s[last:m[0]]...)
		out = p.re.ExpandString(out, with, s, m)
		last = m[1]
		if len(out) > 4*limit {
			return tooLong()
		}
	}
	out = append(out, s[last:]...)
	e.scan(len(out))
	return textResult(string(out), limit)
}
