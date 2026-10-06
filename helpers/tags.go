package helpers

import (
	"strings"
	"unicode/utf8"
)

// A task's tags are kept in its label, comma-separated ("frontend, needs
// review"), so everything that already reads the label (search, the API, AI
// tools, exports) sees them, and a label from before tags is a single tag.

const (
	MaxTags      = 10
	MaxTagLength = 32
)

// Tags splits a label into its tags.
func Tags(label string) []string {
	var out []string
	for _, t := range strings.Split(label, ",") {
		if t = strings.Join(strings.Fields(t), " "); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// NormaliseTags is a label as stored: tags trimmed, inner spaces collapsed,
// each cut to MaxTagLength, duplicates (ignoring case) dropped keeping the
// first spelling, at most MaxTags, joined by ", ".
func NormaliseTags(label string) string {
	seen := map[string]bool{}
	var keep []string
	for _, t := range Tags(label) {
		if utf8.RuneCountInString(t) > MaxTagLength {
			t = strings.TrimSpace(string([]rune(t)[:MaxTagLength]))
		}
		k := strings.ToLower(t)
		if seen[k] {
			continue
		}
		seen[k] = true
		keep = append(keep, t)
		if len(keep) == MaxTags {
			break
		}
	}
	return strings.Join(keep, ", ")
}

// TagChanges is what moving from one label to another adds and removes.
func TagChanges(from, to string) (added, removed []string) {
	in := func(list []string, t string) bool {
		for _, x := range list {
			if strings.EqualFold(x, t) {
				return true
			}
		}
		return false
	}
	old, next := Tags(from), Tags(to)
	for _, t := range next {
		if !in(old, t) {
			added = append(added, t)
		}
	}
	for _, t := range old {
		if !in(next, t) {
			removed = append(removed, t)
		}
	}
	return added, removed
}

// WithTag is the label with tag added (unchanged when it is already there).
func WithTag(label, tag string) string {
	return NormaliseTags(label + "," + tag)
}

// WithoutTag is the label with tag removed, ignoring case.
func WithoutTag(label, tag string) string {
	var keep []string
	for _, t := range Tags(label) {
		if !strings.EqualFold(t, strings.Join(strings.Fields(tag), " ")) {
			keep = append(keep, t)
		}
	}
	return NormaliseTags(strings.Join(keep, ","))
}
