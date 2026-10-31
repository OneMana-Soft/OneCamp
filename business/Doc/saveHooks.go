package businness

// Save Hooks: what runs after a doc's body is saved from the collaboration
// service (docs programme, PROGRAM.md §4 #11 and invariant I15; A4 built the
// change detection and the Snapshot Service's hook, A6 adds the mentions).
//
// A hook registers from its package's init():
//
//	func init() { docBusiness.SaveHooks.Register("doc.mentions", notifyMentions) }
//
// SaveFromCollab (docCollabSave.go) runs the hooks once per save that CHANGED
// the body, after it is stored, in the background: a hook never delays or fails a
// save, and a save that changed nothing anyone can see (an edit undone, a
// state-only save) runs none (invariant I15). Each hook runs on its own, in no set
// order, so one that panics or is slow holds up no other. Change carries what
// changed: the body before and after by hash, the lineage, and who made it.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// AgentContributor is an agent whose edit a save holds, with the person who
// asked it (I11).
type AgentContributor struct {
	AgentUUID string `json:"agent_uuid"`
	AskerUUID string `json:"asker_uuid"`
}

// Change is one save that changed a doc's body.
type Change struct {
	DocUUID string
	// Body is the body as saved.
	Body string
	// Previous is the body before the save, read only when the save shrank it
	// sharply (a mass deletion, which history keeps); "" otherwise.
	Previous string
	// Before is the body before the save whenever the save path read it: for
	// a mass deletion, or because a hook asked for it (AskForBefore); ""
	// otherwise.
	Before string
	// Hash and PrevHash are the body's hashes after and before (collabBodyHash).
	Hash     string
	PrevHash string
	// Lineage is the copy of the doc the save came from (the collaboration
	// service's epoch; "" for a doc never rebuilt from its HTML).
	Lineage string
	// Contributors are the people whose edits the save holds, and Agents the
	// agents, each with the person who asked it.
	Contributors []string
	Agents       []AgentContributor
	SavedAt      time.Time
	// Hosted is a doc that belongs to something else (a table row's page,
	// D-18): only the hooks registered for hosted docs run for it
	// (RegisterAlsoHosted), so a hook added later puts no hosted doc where
	// docs are found (the AI index, mention notices) without asking to.
	Hosted bool
	// Quiet is a save holding a quiet write (a copy's body, an import's): who
	// its body mentions is kept and nobody is told (docMentions.go), and its
	// hooks and its doc.updated run as a bulk import's do
	// (helpers.BulkImportContextKey), so of the event sinks only those that
	// take an import's events hear it.
	Quiet bool
}

// SaveHook is one thing to do after a save.
type SaveHook func(ctx context.Context, c Change)

// SaveHookRegistry holds the hooks.
type SaveHookRegistry struct {
	mu    sync.RWMutex
	hooks []namedSaveHook
	asks  []BeforeAsk
}

// BeforeAsk answers, before a save is written, whether a hook will need the
// body as it was (Change.Before). It is asked on every changed save, so it is
// cheap and answers true only when its hook will keep the body: the read it
// asks for costs a round trip.
type BeforeAsk func(ctx context.Context, docUUID string, contributors []string, agents []AgentContributor) bool

// AskForBefore adds a BeforeAsk.
func (r *SaveHookRegistry) AskForBefore(ask BeforeAsk) {
	if ask == nil {
		panic("save hooks: AskForBefore needs something to ask")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asks = append(r.asks, ask)
}

// wantsBefore is whether any hook asked for the body before this save.
func (r *SaveHookRegistry) wantsBefore(ctx context.Context, docUUID string, contributors []string, agents []AgentContributor) bool {
	r.mu.RLock()
	asks := append([]BeforeAsk(nil), r.asks...)
	r.mu.RUnlock()
	for _, ask := range asks {
		if ask(ctx, docUUID, contributors, agents) {
			return true
		}
	}
	return false
}

type namedSaveHook struct {
	name string
	run  SaveHook
	// hosted: it runs for a hosted doc's saves too.
	hosted bool
}

// SaveHooks is the doc save path's registry.
var SaveHooks = &SaveHookRegistry{}

// Register adds a hook under a name of its own. A nil hook, or a name already
// taken, panics: registration happens in init, where that fails the build's tests
// instead of leaving a hook that never runs.
func (r *SaveHookRegistry) Register(name string, hook SaveHook) {
	r.register(name, hook, false)
}

// RegisterAlsoHosted adds a hook that runs for a hosted doc's saves too (a
// table row's page, D-18): one that keeps the doc, its versions say, and puts
// it nowhere docs are found.
func (r *SaveHookRegistry) RegisterAlsoHosted(name string, hook SaveHook) {
	r.register(name, hook, true)
}

func (r *SaveHookRegistry) register(name string, hook SaveHook, hosted bool) {
	if name == "" || hook == nil {
		panic(fmt.Sprintf("save hook %q needs a name and something to run", name))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range r.hooks {
		if h.name == name {
			panic(fmt.Sprintf("save hook %q is registered twice", name))
		}
	}
	r.hooks = append(r.hooks, namedSaveHook{name: name, run: hook, hosted: hosted})
}

// run starts every hook for c and returns at once. The hooks keep ctx's values
// but not its cancellation: the save's request ends before they do.
func (r *SaveHookRegistry) run(ctx context.Context, c Change) {
	r.mu.RLock()
	hooks := append([]namedSaveHook(nil), r.hooks...)
	r.mu.RUnlock()
	if len(hooks) == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	for _, h := range hooks {
		if c.Hosted && !h.hosted {
			continue
		}
		// Recovered, so a hook that panics doesn't take the server down, and
		// counted, so a shutdown or a test waits for it (helpers.GoSafeNamed).
		helpers.GoSafeNamed("doc save hook "+h.name+" for doc "+c.DocUUID, func() { h.run(ctx, c) })
	}
}
