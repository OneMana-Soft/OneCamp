package business

// Resources: how threads reach each kind of resource. The core (service.go)
// keeps threads, participants, permissions and events; a Resource answers
// what only the resource knows: who may do what there, where its comments
// live, and which anchors it takes. A kind registers once, from an init:
//
//	func init() { threadBusiness.Resources.Register("channel", channelThreads{}) }
//
// Docs are registered here (docResource.go). The kinds the programme names
// next (a chat message, an event, a call's timeline, a table cell, a task)
// each bring their own Resource and keep their comments where they already
// live; nothing in the threads table is particular to any of them.

import (
	"context"
	"fmt"
	"sync"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
)

// Posted is a comment just written, and who it mentions (user uuids).
type Posted struct {
	Comment   Comment
	Mentioned []string
}

// Resource is one kind of resource, as threads use it.
type Resource interface {
	// Anchors are the kinds of anchor a thread on this kind of resource takes.
	Anchors() []AnchorKind
	// Access is who's decision on res, from the Access Resolver. ErrNotFound
	// for no such resource.
	Access(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource) (accessBusiness.Decision, error)
	// Comments are res's live comments, each with its thread id ("" for a
	// comment in no thread). ErrNotFound for no such resource.
	Comments(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource) ([]Comment, error)
	// Post writes a comment by who into thread threadID on res, with whatever
	// the resource does for a new comment (search, notifications, activity).
	Post(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource, threadID string, body Body) (Posted, error)
	// Legacy finds a comment written before threads by its uuid, and the
	// page-level thread it reads as (its id the comment's). ok is false when
	// the uuid is no such comment, or one already in another thread.
	Legacy(ctx context.Context, commentID string) (t Thread, root Comment, ok bool, err error)
	// Claim puts a legacy comment in the thread made from it.
	Claim(ctx context.Context, threadID string, root Comment) error
}

// Registry is the kinds threads can reach.
type Registry struct {
	mu     sync.RWMutex
	byKind map[string]Resource
}

// Resources is where every kind registers.
var Resources = &Registry{byKind: map[string]Resource{}}

// Register adds a kind. A second registration of one kind is a mistake two
// packages made in parallel, and panics where it happened.
func (r *Registry) Register(kind string, res Resource) {
	if kind == "" || res == nil {
		panic("threads: a resource kind needs a name and an adapter")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, taken := r.byKind[kind]; taken {
		panic(fmt.Sprintf("threads: the resource kind %q is registered twice", kind))
	}
	r.byKind[kind] = res
}

// of is a kind's adapter.
func (r *Registry) of(kind string) (Resource, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	res, ok := r.byKind[kind]
	return res, ok
}

// kinds are the registered kinds and their adapters, in no set order.
func (r *Registry) kinds() map[string]Resource {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]Resource, len(r.byKind))
	for k, v := range r.byKind {
		out[k] = v
	}
	return out
}

func init() { Resources.Register(accessBusiness.KindDoc, docResource{}) }
