package codepr

// Context fusion — assembling the org's *why* (the originating conversation, the
// linked task, referenced docs, prior PRs, workspace memory) into a bounded,
// deduped, prioritized block that grounds the coding prompt alongside the
// runner's CODE retrieval. This is OneCamp's structural edge: a repo-only or
// cloud agent can't see the discussion that produced the task.
//
// This file is the PURE, fully-tested core: given already-gathered,
// permission-checked fragments + a budget, it produces the deterministic bundle.
// The gatherers (DB/permission layer) and the orchestration wiring are separate
// slices (see .kiro/specs/context-fusion-retrieval). OFF-safe by construction:
// zero fragments → "" → the code-only prompt is unchanged.

import (
	"fmt"
	"sort"
	"strings"
)

// FragmentKind classifies one piece of org context.
type FragmentKind string

const (
	KindOriginConversation FragmentKind = "conversation"
	KindLinkedTask         FragmentKind = "task"
	KindDoc                FragmentKind = "doc"
	KindPriorPR            FragmentKind = "prior_pr"
	KindMemory             FragmentKind = "memory"
)

// defaultKindPriority ranks kinds when weights tie: the conversation that
// produced the task and the linked task carry the most intent, then prior PRs on
// the same code, then design docs, then general memory. Lower = earlier.
var defaultKindPriority = map[FragmentKind]int{
	KindOriginConversation: 0,
	KindLinkedTask:         1,
	KindPriorPR:            2,
	KindDoc:                3,
	KindMemory:             4,
}

// kindLabel is the human section header for a kind in the rendered bundle.
var kindLabel = map[FragmentKind]string{
	KindOriginConversation: "Originating conversation",
	KindLinkedTask:         "Linked task",
	KindDoc:                "Referenced doc",
	KindPriorPR:            "Prior related pull request",
	KindMemory:             "Workspace knowledge",
}

// ContextFragment is one bounded, permission-checked piece of org context.
// Weight is caller-assigned relevance (higher = more relevant); ties fall back
// to kind priority then input order, so assembly is fully deterministic.
type ContextFragment struct {
	Kind   FragmentKind
	Title  string
	Body   string
	Weight int
}

// SourceRef names a fragment that was actually included, for PR-body / transcript
// transparency (kind + title only — never restricted raw content).
type SourceRef struct {
	Kind  FragmentKind
	Title string
}

// minFragmentChars is the floor for a fragment's fair share, so even a tight
// budget still includes a useful head of each high-signal fragment rather than
// nothing.
const minFragmentChars = 400

// AssembleContext turns permission-checked fragments into the coding-prompt
// context bundle. It is PURE and deterministic:
//   - drops empty fragments,
//   - de-duplicates by (kind, normalized body),
//   - orders by weight desc, then kind priority, then input order,
//   - bounds each fragment to a fair share of the budget (head-truncated),
//   - stops when the budget is exhausted and discloses "[N more … omitted]",
//   - renders each with a clear section header.
//
// Returns the bundle and the SourceRefs actually included (for transparency).
// Zero usable fragments or a non-positive budget yields ("", nil), so the
// caller's prompt is byte-identical to the code-only path (OFF-safe).
func AssembleContext(fragments []ContextFragment, budgetChars int) (string, []SourceRef) {
	if budgetChars <= 0 {
		return "", nil
	}

	// 1. Clean + dedup, preserving input order for stable tie-breaking.
	seen := make(map[string]bool, len(fragments))
	cleaned := make([]ContextFragment, 0, len(fragments))
	for _, f := range fragments {
		body := strings.TrimSpace(f.Body)
		if body == "" {
			continue
		}
		f.Body = body
		f.Title = strings.TrimSpace(f.Title)
		key := string(f.Kind) + "\x00" + strings.ToLower(strings.Join(strings.Fields(body), " "))
		if seen[key] {
			continue
		}
		seen[key] = true
		cleaned = append(cleaned, f)
	}
	if len(cleaned) == 0 {
		return "", nil
	}

	// 2. Stable order: weight desc → kind priority asc → input order.
	idx := make(map[*ContextFragment]int, len(cleaned))
	ordered := make([]*ContextFragment, len(cleaned))
	for i := range cleaned {
		ordered[i] = &cleaned[i]
		idx[&cleaned[i]] = i
	}
	sort.SliceStable(ordered, func(a, b int) bool {
		fa, fb := ordered[a], ordered[b]
		if fa.Weight != fb.Weight {
			return fa.Weight > fb.Weight
		}
		pa, pb := defaultKindPriority[fa.Kind], defaultKindPriority[fb.Kind]
		if pa != pb {
			return pa < pb
		}
		return idx[fa] < idx[fb]
	})

	// 3. Per-fragment fair share of the budget (floored), so one huge thread
	//    can't crowd out the rest.
	fair := budgetChars / len(ordered)
	if fair < minFragmentChars {
		fair = minFragmentChars
	}

	var b strings.Builder
	var used []SourceRef
	remaining := budgetChars
	omitted := 0

	for _, f := range ordered {
		header := sectionHeader(f)
		body := f.Body
		slotCap := fair
		if slotCap > remaining {
			slotCap = remaining
		}
		// Reserve room for the header; if we can't fit the header + a minimal
		// slice of body, the rest is omitted.
		if remaining <= len(header)+minFragmentChars/4 {
			omitted++
			continue
		}
		bodyCap := slotCap - len(header)
		if bodyCap < 0 {
			bodyCap = 0
		}
		if len(body) > bodyCap {
			body = strings.TrimSpace(body[:bodyCap]) + " …[truncated]"
		}
		section := header + body + "\n\n"
		b.WriteString(section)
		remaining -= len(section)
		used = append(used, SourceRef{Kind: f.Kind, Title: f.Title})
		if remaining <= minFragmentChars/4 {
			// No meaningful room left; count the rest as omitted.
			omitted += remainingCount(ordered, f)
			break
		}
	}

	if omitted > 0 {
		fmt.Fprintf(&b, "[%d more context item%s omitted to fit the budget]\n", omitted, plural(omitted))
	}

	return strings.TrimRight(b.String(), "\n") + "\n", used
}

// sectionHeader renders the labeled header line for a fragment.
func sectionHeader(f *ContextFragment) string {
	label := kindLabel[f.Kind]
	if label == "" {
		label = "Context"
	}
	if f.Title != "" {
		return fmt.Sprintf("### %s: %s\n", label, f.Title)
	}
	return fmt.Sprintf("### %s\n", label)
}

// remainingCount returns how many fragments come AFTER `cur` in `ordered` (used
// to disclose how many were dropped when the budget runs out mid-list).
func remainingCount(ordered []*ContextFragment, cur *ContextFragment) int {
	for i, f := range ordered {
		if f == cur {
			return len(ordered) - i - 1
		}
	}
	return 0
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
