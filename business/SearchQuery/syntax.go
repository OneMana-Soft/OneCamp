// Package business (SearchQuery) is the one query grammar of docs, chat,
// projects and tables (design/QUERY_STACK_CONTRACT.md §4, signed by the docs,
// projects and tables leads on 17 Oct 2026): Slack's modifiers
// (from:@sam in:#launch before:friday "pricing page" -draft) and JQL's compare
// form (project = Q4 AND status != Done ORDER BY due) in one text.
//
//   - Parse reads a text into its syntax tree: pure, needing no registry, and
//     never failing. The web's typing twin, lib/search/query.ts, reads the
//     same text into the same tree; the fixtures in testdata/searchquery
//     hold the two to it.
//   - Build reads the syntax tree as the asker into the one typed tree every
//     compiler reads (business/QuerySpec): fields, names and values resolved
//     through the registry and the Access Resolver, symbols read as operator
//     names.
//   - Format writes a tree back as text.
//
// The server's parser alone decides what a text means: the web's twin only
// colours tokens and draws chips, and the server re-parses every text it is
// given.
package business

import querySpec "github.com/akashc777/OneCamp/business/QuerySpec"

// Syntax is a text as parsed: its tree, its ORDER BY, what in it couldn't be
// read, and its tokens, for colour and chips.
type Syntax struct {
	Root   *Node     `json:"root,omitempty"`
	Sorts  []SortKey `json:"sorts,omitempty"`
	Issues []Issue   `json:"issues,omitempty"`
	Tokens []Token   `json:"tokens,omitempty"`
}

// Node is one piece of a parsed text.
type Node struct {
	// Kind is NodeAnd, NodeOr or NodeNot (a group), NodeWord, NodePhrase, or
	// NodeClause (a field compared with values).
	Kind string `json:"kind"`
	// Children are a group's: two or more for and and or, one for not.
	Children []*Node `json:"children,omitempty"`
	// Text is a word's or a phrase's.
	Text string `json:"text,omitempty"`

	// A clause's field as written, and whether it was quoted (a custom field
	// by its name).
	Field  string `json:"field,omitempty"`
	Quoted bool   `json:"quoted,omitempty"`
	// Form is querySpec.FormModifier (from:@sam) or querySpec.FormCompare
	// (status = done).
	Form string `json:"form,omitempty"`
	// Op is the symbol as written (the Sym names below).
	Op string `json:"op,omitempty"`
	// Values are as typed, without their quotes: none for an empty modifier
	// and for IS EMPTY, one or more for a list.
	Values []string `json:"values,omitempty"`
	// List is a clause whose values were written as a list: in (a, b), and
	// = (a, b), which is "exactly these".
	List bool `json:"list,omitempty"`

	// Span is where the node sits in the text, [from, to) in UTF-16 code
	// units.
	Span querySpec.Span `json:"span"`
}

// The kinds of node.
const (
	NodeAnd    = "and"
	NodeOr     = "or"
	NodeNot    = "not"
	NodeWord   = "word"
	NodePhrase = "phrase"
	NodeClause = "clause"
)

// The symbols a clause is written with (Node.Op).
const (
	SymModifier    = ":"
	SymEq          = "="
	SymNe          = "!="
	SymLt          = "<"
	SymLe          = "<="
	SymGt          = ">"
	SymGe          = ">="
	SymContains    = "~"
	SymNotContains = "!~"
	SymIn          = "in"
	SymNotIn       = "not_in"
	SymEmpty       = "empty"
	SymNotEmpty    = "not_empty"
)

// SortKey is one key of an ORDER BY.
type SortKey struct {
	Field  string         `json:"field"`
	Quoted bool           `json:"quoted,omitempty"`
	Desc   bool           `json:"desc,omitempty"`
	Span   querySpec.Span `json:"span"`
}

// Issue is something about a text that couldn't be read as written, where it
// is, and once built how much it matters.
type Issue struct {
	Code string         `json:"code"`
	Span querySpec.Span `json:"span"`
	// Level, Message and Suggestions are Build's: Parse leaves them empty, so
	// the parsers in Go and TypeScript agree on codes and places alone.
	Level       string   `json:"level,omitempty"`
	Message     string   `json:"message,omitempty"`
	Suggestions []string `json:"suggestions,omitempty"`
}

// The issues Parse finds.
const (
	IssueUnclosedQuote = "unclosed_quote"
	IssueUnclosedParen = "unclosed_paren"
	IssueStrayParen    = "stray_paren"
	IssueEmptyValue    = "empty_value"
	IssueTooDeep       = "too_deep"
	IssueTooLong       = "too_long"
	IssueTooManySorts  = "too_many_sorts"
	IssueAfterOrder    = "after_order"
	IssueBadList       = "bad_list"
)

// Token is a stretch of the text and what it is, for colour and chips.
type Token struct {
	Kind string         `json:"kind"`
	Span querySpec.Span `json:"span"`
}

// The kinds of token.
const (
	TokenField   = "field"
	TokenOp      = "op"
	TokenValue   = "value"
	TokenWord    = "word"
	TokenPhrase  = "phrase"
	TokenKeyword = "keyword"
	TokenNeg     = "neg"
	TokenParen   = "paren"
	TokenComma   = "comma"
)

// The limits of a text (design/QUERY_STACK_CONTRACT.md §4.2).
const (
	// MaxText is the longest text, in UTF-16 code units.
	MaxText = 1000
	// MaxBrackets is how deep brackets nest.
	MaxBrackets = 3
)
