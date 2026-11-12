package business

// Code names tasks (P10-1a): a commit message, a pull request's title or body,
// or a branch's name names a task by its key ("WEB-12", any key or alias
// ParseSource reads, in any case) or by its address (task/<id>, as before
// keys), and a closing word before one or several of them ("Fixes WEB-1,
// WEB-2") says the change closes them. This is the one reading of it, for
// every code host: which task a ref is, and whether an event on a repository
// may change it, is its caller's question (business/GitHub codeTask).

import (
	"regexp"
	"sort"
	"strconv"

	"github.com/google/uuid"
)

// MaxRefs is the most refs one text yields: each key is a read, and a commit
// message is whoever pushes it's to write.
const MaxRefs = 50

// CodeRef is a task code names: by its key, or by its id.
type CodeRef struct {
	// Key is the key as named, in capitals ("WEB-12"); "" for an id.
	Key string
	// ID is the task's id, named by its address; uuid.Nil for a key.
	ID uuid.UUID
	// Close is a closing word before it: "fixes WEB-12".
	Close bool
}

var (
	// A task's address, with or without the app's address before it.
	idRef = regexp.MustCompile(`(?:https?://[^\s<>()"']*?/)?task/([0-9a-fA-F-]{36})`)
	// A uuid anywhere, which is never read as keys ("a1b2c3d4-5678-…").
	anyUUID = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	// What may be a key: a prefix ParseSource checks, a dash and a number.
	keyRef = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9]{0,9}-[1-9][0-9]{0,8}\b`)
	// The words that close what follows them.
	closing = regexp.MustCompile(`(?i)\b(?:fix(?:e[sd])?|close[sd]?|resolve[sd]?)\b`)
	// What may stand between a closing word and its first ref (a colon), and
	// between refs in its list (a comma, "and", "&").
	listed = regexp.MustCompile(`(?i)^\s*:?[\s,&]*(?:and[\s,]+)?$`)
)

// RefsIn is the tasks a commit message or a pull request's title or body
// names, in the order first named, each once: closed when any of its mentions
// follows a closing word. At most MaxRefs.
func RefsIn(text string) []CodeRef {
	type found struct {
		ref        CodeRef
		start, end int
	}
	var all []found
	var masked [][]int
	for _, m := range idRef.FindAllStringSubmatchIndex(text, -1) {
		masked = append(masked, m[:2])
		if id, err := uuid.Parse(text[m[2]:m[3]]); err == nil {
			all = append(all, found{CodeRef{ID: id}, m[0], m[1]})
		}
	}
	masked = append(masked, anyUUID.FindAllStringIndex(text, -1)...)
	for _, m := range keyRef.FindAllStringIndex(text, -1) {
		if overlaps(m, masked) {
			continue
		}
		if k, n, ok := ParseSource(text[m[0]:m[1]]); ok {
			all = append(all, found{CodeRef{Key: k + "-" + strconv.Itoa(n)}, m[0], m[1]})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].start < all[j].start })
	// Each closing word closes the refs listed right after it.
	for _, c := range closing.FindAllStringIndex(text, -1) {
		at := c[1]
		for i := range all {
			if all[i].start < at {
				continue
			}
			if !listed.MatchString(text[at:all[i].start]) {
				break
			}
			all[i].ref.Close = true
			at = all[i].end
		}
	}
	var out []CodeRef
	seen := map[CodeRef]int{}
	for _, f := range all {
		named := CodeRef{Key: f.ref.Key, ID: f.ref.ID}
		if i, ok := seen[named]; ok {
			out[i].Close = out[i].Close || f.ref.Close
			continue
		}
		if len(out) == MaxRefs {
			continue
		}
		seen[named] = len(out)
		out = append(out, f.ref)
	}
	return out
}

// BranchRefs is the tasks a branch's name names ("feature/WEB-12-export",
// "web-12-export", "task/<id>-hotfix"). A branch's name never closes a task:
// "fix/WEB-3" names WEB-3.
func BranchRefs(branch string) []CodeRef {
	refs := RefsIn(branch)
	for i := range refs {
		refs[i].Close = false
	}
	return refs
}

// overlaps is whether span m overlaps any of spans. Pure.
func overlaps(m []int, spans [][]int) bool {
	for _, s := range spans {
		if m[0] < s[1] && s[0] < m[1] {
			return true
		}
	}
	return false
}
