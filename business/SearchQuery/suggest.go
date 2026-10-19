package business

// Suggest: what to type at the caret (design/QUERY_STACK_CONTRACT.md §4.8):
// field names while a word is being typed, and a field's values after its
// colon: its fixed values, me and @my_team for people, and day names for
// dates. People, places and options by name are suggested once the search
// engine lands on the grammar (docs C2-1), through the same resolvers.

import (
	"context"
	"sort"
	"strings"

	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
)

// Suggestion is something to type at the caret.
type Suggestion struct {
	// Kind is SuggestField or SuggestValue.
	Kind  string `json:"kind"`
	Label string `json:"label"`
	// Insert is what replaces Span, the stretch being typed.
	Insert string         `json:"insert"`
	Detail string         `json:"detail,omitempty"`
	Span   querySpec.Span `json:"span"`
}

// The kinds of suggestion.
const (
	SuggestField = "field"
	SuggestValue = "value"
)

// maxSuggestions is how many suggestions one answer holds.
const maxSuggestions = 8

// The dates a date field suggests, in the order people reach for them.
var suggestedDates = []string{"today", "yesterday", "this_week", "last_week", "this_month", "last_month", "past_7_days", "past_30_days", "-7d"}

// Suggest is what to type at caret, a UTF-16 offset into text.
func Suggest(ctx context.Context, who Asker, text string, caret int, scope querySpec.Scope) []Suggestion {
	syn := Parse(text)
	src := newSource(text)
	if caret < 0 || caret > src.u16[len(src.runes)] {
		return nil
	}
	// The token the caret ends or sits in.
	at := -1
	for i, t := range syn.Tokens {
		if t.Span[0] < caret && caret <= t.Span[1] {
			at = i
		}
	}
	if at < 0 {
		return nil
	}
	t := syn.Tokens[at]
	switch t.Kind {
	case TokenWord:
		typed := spanText(src, querySpec.Span{t.Span[0], caret})
		return fieldSuggestions(ctx, who, scope, typed, querySpec.Span{t.Span[0], t.Span[1]})
	case TokenOp:
		// Just after a modifier's colon: the field's values.
		if at > 0 && syn.Tokens[at-1].Kind == TokenField && caret == t.Span[1] {
			field := spanText(src, syn.Tokens[at-1].Span)
			return valueSuggestions(ctx, who, scope, field, "", querySpec.Span{caret, caret})
		}
	case TokenValue:
		for j := at - 1; j >= 0; j-- {
			if syn.Tokens[j].Kind == TokenField {
				field := strings.Trim(spanText(src, syn.Tokens[j].Span), `"`)
				typed := spanText(src, querySpec.Span{t.Span[0], caret})
				return valueSuggestions(ctx, who, scope, field, typed, t.Span)
			}
		}
	}
	return nil
}

func spanText(src source, span querySpec.Span) string {
	from := sort.SearchInts(src.u16, span[0])
	to := sort.SearchInts(src.u16, span[1])
	return src.text(from, to)
}

func fieldSuggestions(ctx context.Context, who Asker, scope querySpec.Scope, typed string, span querySpec.Span) []Suggestion {
	prefix := strings.ToLower(typed)
	if prefix == "" {
		return nil
	}
	var out []Suggestion
	for _, name := range Fields.names() {
		f, _ := Fields.lookup(name)
		if name == "text" || !strings.HasPrefix(name, prefix) || name == prefix {
			continue
		}
		out = append(out, Suggestion{Kind: SuggestField, Label: name + ":", Insert: name + ":", Detail: f.Help, Span: span})
	}
	if provider, ok := Fields.scopeFields(scope.Kind); ok {
		custom, _ := provider.Fields(ctx, who, scope)
		for _, f := range custom {
			if strings.HasPrefix(strings.ToLower(f.Name), prefix) {
				out = append(out, Suggestion{Kind: SuggestField, Label: f.Name, Insert: quote(f.Name) + ":", Detail: f.Help, Span: span})
			}
		}
	}
	if len(out) > maxSuggestions {
		out = out[:maxSuggestions]
	}
	return out
}

func valueSuggestions(ctx context.Context, who Asker, scope querySpec.Scope, field, typed string, span querySpec.Span) []Suggestion {
	b := &builder{ctx: ctx, who: who, scope: scope}
	if provider, ok := Fields.scopeFields(scope.Kind); ok {
		b.custom, b.customErr = provider.Fields(ctx, who, scope)
	}
	f, problem := b.field(field, false)
	if problem != "" {
		return nil
	}
	if f.Target != "" {
		if target, ok := Fields.lookup(f.Target); ok {
			f = target
		}
	}
	prefix := strings.ToLower(strings.TrimSpace(typed))
	var candidates []Suggestion
	add := func(value, label, detail string) {
		if prefix != "" && !strings.HasPrefix(strings.ToLower(value), prefix) && !strings.HasPrefix(strings.ToLower(label), prefix) {
			return
		}
		insert := value
		if needsQuote(value) {
			insert = quote(value)
		}
		candidates = append(candidates, Suggestion{Kind: SuggestValue, Label: label, Insert: insert, Detail: detail, Span: span})
	}
	switch {
	case len(f.Choices) > 0:
		for _, c := range f.Choices {
			label := c.Label
			if label == "" {
				label = c.Value
			}
			add(c.Value, label, "")
		}
	case f.Family == FamilyPeople:
		add("me", "me", "You")
		add("@my_team", "@my_team", "Anyone in a team you're in")
	case f.Family == FamilyDate:
		for _, d := range suggestedDates {
			add(d, strings.ReplaceAll(d, "_", " "), "")
		}
	}
	if len(candidates) > maxSuggestions {
		candidates = candidates[:maxSuggestions]
	}
	return candidates
}
