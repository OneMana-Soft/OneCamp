package ai

import (
	"sort"
	"strings"
	"sync"
)

// Who reports that a capability is configured but unreachable.
//
// Registration inversion, the same shape the retention sweepers and evidence
// contributors use: a subsystem that can be degraded registers itself, and
// nothing here needs to know what subsystems exist. The AI-free edition links
// no reporters and the loop below runs over an empty map.
//
// Reporters are asked at request time rather than pushing at failure time on
// purpose. The failure is discovered when the tool registry is rebuilt, which
// happens on a background context belonging to nobody; the person who needs to
// hear about it turns up minutes later on a different context entirely.
type degradationReporter struct {
	name string
	list func() []DegradedCapability
}

// DegradedCapability is one thing that is configured, enabled, and giving this
// answer nothing.
//
// Topics is what a question would have to be about for this to have mattered.
// Without it every answer in the workspace carried the same warning, including
// the ones the connector could not have helped with, and a notice that appears
// on every answer is one nobody reads by the time it means something. Empty
// topics means "we cannot tell", and an unknown is always reported: a person
// who was told about a thinner answer can judge it, and a person who was not
// told cannot.
type DegradedCapability struct {
	Name   string
	Topics []string
}

var (
	degradationMu        sync.RWMutex
	degradationReporters = map[string]degradationReporter{}
)

// RegisterDegradationReporter records a source of "configured but unreachable"
// capabilities. Re-registering a name replaces it, so a package that
// re-registers on reload does not accumulate duplicates.
func RegisterDegradationReporter(name string, list func() []DegradedCapability) {
	if name == "" || list == nil {
		return
	}
	degradationMu.Lock()
	defer degradationMu.Unlock()
	degradationReporters[name] = degradationReporter{name: name, list: list}
}

// degradations asks every reporter what is unreachable.
//
// Sorted by name, because the sentence a person reads should not reorder itself
// between two identical questions.
func degradations() []DegradedCapability {
	degradationMu.RLock()
	reporters := make([]degradationReporter, 0, len(degradationReporters))
	for _, r := range degradationReporters {
		reporters = append(reporters, r)
	}
	degradationMu.RUnlock()

	var all []DegradedCapability
	for _, r := range reporters {
		all = append(all, r.list()...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return all
}

// relevantDegradations names the capabilities worth telling somebody about,
// given what they asked.
//
// An empty question means nobody said what this answer was about, so
// everything is named: the conservative direction, and what every path did
// before a question could be recorded. A capability with no topics is named
// for the same reason.
//
// De-duplicated by name, so two reporters naming the same connector produce
// one mention rather than two. Pure, so the rule is testable without a
// registry.
func relevantDegradations(caps []DegradedCapability, question string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, c := range caps {
		name := strings.TrimSpace(c.Name)
		if name == "" || seen[name] {
			continue
		}
		if strings.TrimSpace(question) != "" && len(c.Topics) > 0 && !containsWholeWord(question, c.Topics) {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// TopicsForCapability is what a question has to mention for a named capability
// to have been relevant to it.
//
// Three sources, because no one of them is enough on its own:
//
//   - the capability's own name, which catches "is GitHub connected";
//   - the words in the tool names it would have contributed, which catches the
//     vocabulary of whatever it does without anybody curating a list;
//   - the keywords already curated for the built-in tools of the same name,
//     which catches the way people actually ask. Nobody types "GitHub" to ask
//     "what PRs are open", and that curated table already knows it.
//
// Generic verbs are dropped. A connector whose tools are called get_, list_ and
// create_ would otherwise be relevant to every sentence ever typed, which is
// the noise this exists to remove.
func TopicsForCapability(name string, toolNames []string) []string {
	seen := map[string]bool{}
	add := func(w string) {
		w = strings.ToLower(strings.TrimSpace(w))
		if len(w) < 3 || genericToolWords[w] {
			return
		}
		seen[w] = true
	}

	lowerName := strings.ToLower(strings.TrimSpace(name))
	for _, w := range strings.Fields(lowerName) {
		add(w)
	}
	for _, t := range toolNames {
		for _, w := range strings.FieldsFunc(t, func(r rune) bool {
			return r == '_' || r == '-' || r == '.' || r == '/' || r == ' '
		}) {
			add(w)
		}
	}
	// The curated vocabulary for built-in tools of the same name. A connector
	// called GitHub and the github_* tools answer the same kinds of question.
	for _, g := range toolGroups {
		for _, k := range g.keywords {
			if k == lowerName {
				for _, kw := range g.keywords {
					add(kw)
				}
				break
			}
		}
	}

	out := make([]string, 0, len(seen))
	for w := range seen {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

// genericToolWords are the words that say what a tool does to something rather
// than what it is about. A connector is not relevant to a question because the
// question contains "get".
var genericToolWords = map[string]bool{
	"get": true, "list": true, "create": true, "update": true, "delete": true,
	"add": true, "remove": true, "set": true, "search": true, "find": true,
	"fetch": true, "read": true, "write": true, "send": true, "run": true,
	"the": true, "and": true, "for": true, "all": true, "new": true, "any": true,
	"tool": true, "tools": true, "api": true, "mcp": true, "server": true,
}
