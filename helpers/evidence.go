package helpers

import (
	"context"
	"sort"
	"sync"
	"time"
)

// A registry of records that belong in an evidence pack.
//
// WHY AN INVERSION, again. The pack is assembled in the audit package, which
// both editions ship. The agent run ledger is one of its sections and lives in
// the AI packages, which only one edition ships. Calling it directly would not
// be a style argument, it would be a build failure on the AI-free edition, where
// those packages are not present. Same reasoning as the retention registry next
// door, and deliberately the same shape so there is one pattern to learn.
//
// A section announces itself from its own package's init, so linking the package
// is what puts its records in the pack, and not linking it is what leaves them
// out. The pack then reports which sections it actually contains, which matters:
// an auditor reading a pack with no agent section should be able to tell "this
// deployment runs no agents" apart from "somebody removed the agents".
type EvidenceSection struct {
	// Name is what the section is called in the pack and in its manifest entry.
	Name string
	// Describe is one sentence on what the section contains and what it proves,
	// carried into the pack so the document explains itself to a reader who has
	// never seen this system.
	Describe string
	// Collect returns the rows for a time window. Any JSON-marshalable value.
	Collect func(ctx context.Context, from, to time.Time) (any, error)

	// Contextual marks a section that describes the DEPLOYMENT rather than the
	// window: the retention policy in force, say, which is the same one row
	// whether the month held ten thousand records or none.
	//
	// WHY ANYONE NEEDS TO KNOW. A caller asking "did anything happen in this
	// window" cannot answer it by counting rows, because a contextual section
	// contributes one on every window forever. The monthly receipt job asked
	// exactly that and got "yes" for nine consecutive empty months, each
	// anchored with an identical fingerprint.
	//
	// Declared by the contributor, because the contributor is the only thing
	// that knows, and carried into the manifest so a reader of the pack can
	// make the same distinction without this registry.
	Contextual bool
}

var (
	evidenceMu      sync.RWMutex
	evidenceSources = map[string]EvidenceSection{}
)

// RegisterEvidenceContributor records a section of the evidence pack. Call it
// from package init. Re-registering a name replaces it, which keeps tests
// hermetic.
func RegisterEvidenceContributor(s EvidenceSection) {
	if s.Name == "" || s.Collect == nil {
		return
	}
	evidenceMu.Lock()
	defer evidenceMu.Unlock()
	evidenceSources[s.Name] = s
}

// EvidenceContributors returns the registered sections in a stable order, so two
// packs covering the same window are byte-identical and their fingerprints can
// be compared.
func EvidenceContributors() []EvidenceSection {
	evidenceMu.RLock()
	out := make([]EvidenceSection, 0, len(evidenceSources))
	for _, s := range evidenceSources {
		out = append(out, s)
	}
	evidenceMu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
