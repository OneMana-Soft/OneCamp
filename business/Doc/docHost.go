package businness

// Hosted docs (docs D-18, amended 17 Oct 2026): a doc that belongs to
// something else, set once when it is made. A table row's page is hosted by
// its row. Its level is its host's, capped at edit, so sharing, moving,
// locking and trashing it are its host's to do; it is in no docs list and no
// docs index, and it is trashed, restored and kept with its host, never on
// its own.
//
// The host is asked only on a path that opens hosted docs for their hosts,
// which its handler marks (WithHostedOpen): the live session (the doc
// session and the legacy authorise), a hosted doc's files (FileAccess's doc
// place), and the host's own read of its doc (a row's page, under /tables).
// Every other path asks the rule alone (domain.DecideDoc), which gives a
// hosted doc none: lists, the tree, home, recents, Starred, the trash,
// templates, moves, sharing, invites, locks, access requests, the doc's MQTT
// topic, MCP, /v1 and export all find it open to nobody, so a docs path added
// later fails closed without knowing hosts exist. A docs package that needs a
// row's page later opens its own path, with a test.
//
// business/Access decides the hosts (hosts.go there: the rule each kind
// registered with it), and sets HostRule from its init, since it imports this
// package. A host kind tells docs where its docs are (RegisterHostKind), for
// the answer a docs path gives someone who may open the host.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/business/Access/level"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// Host is what a hosted doc belongs to: its kind ("table_row") and its id.
type Host struct {
	Kind string
	UUID string
}

// HostDecision is a host's decision on itself for the person a doc's
// decision is for: what the doc's is made from (hostedDecision).
type HostDecision struct {
	Level    level.Level
	Was      level.Level
	Deleted  bool
	Archived bool
}

// HostRule decides hosts for the person v names, a decision per host that is
// there (one that isn't is left out). business/Access sets it.
type HostRule func(ctx context.Context, v Viewer, hosts []Host) (map[Host]HostDecision, error)

var hostRule HostRule

// SetHostRule is called once, from business/Access's init.
func SetHostRule(r HostRule) { hostRule = r }

// hostedOpenKey marks a context whose handler opens hosted docs. Unexported:
// only WithHostedOpen sets it, never anything a request carries.
type hostedOpenKey struct{}

// WithHostedOpen is ctx for a handler that opens hosted docs for their hosts:
// on it, a hosted doc's decision is its host's. Only the paths that open one
// mark it (this file's top says which); nothing a request sends (a header, a
// query, a body) can.
func WithHostedOpen(ctx context.Context) context.Context {
	return context.WithValue(ctx, hostedOpenKey{}, true)
}

// opensHosted is whether ctx is a handler's that opens hosted docs.
func opensHosted(ctx context.Context) bool {
	open, _ := ctx.Value(hostedOpenKey{}).(bool)
	return open
}

// IsHosted is whether a doc belongs to a host. doc must be read with
// doc_host_kind, as every decision read is.
func IsHosted(doc *dgraphStruct.DgraphDoc) bool {
	return domain.IsHosted(doc)
}

// HostOf is the host a doc belongs to, a zero Host for a doc that has none;
// ErrNotFound for no doc.
func HostOf(ctx context.Context, docUUID string) (Host, error) {
	kind, host, err := domain.HostOfDoc(ctx, docUUID)
	if err != nil {
		return Host{}, err
	}
	return Host{Kind: kind, UUID: host}, nil
}

// withHosts gives each hosted doc decided its host's level, on a context that
// opens hosted docs; on any other a hosted doc keeps the rule's none. hosts
// is the host of each hosted doc decided, by lower-case uuid. A host that
// isn't there (gone for good) leaves its doc at none.
func withHosts(ctx context.Context, v Viewer, out map[string]DocDecision, hosts map[string]Host) (map[string]DocDecision, error) {
	if len(hosts) == 0 || !opensHosted(ctx) || hostRule == nil {
		return out, nil
	}
	asked := make([]Host, 0, len(hosts))
	seen := make(map[Host]bool, len(hosts))
	for _, h := range hosts {
		if !seen[h] {
			seen[h] = true
			asked = append(asked, h)
		}
	}
	got, err := hostRule(ctx, v, asked)
	if err != nil {
		return nil, err
	}
	for id, h := range hosts {
		if d, ok := out[id]; ok {
			out[id] = hostedDecision(d, got[h])
		}
	}
	return out, nil
}

// hostedDecision is a hosted doc's decision from its host's: the host's level
// and its level before a deletion, capped at edit (what only full access does
// to a doc is its host's to do), owned by nobody; in the trash when the doc
// or its host is. Pure.
func hostedDecision(doc DocDecision, host HostDecision) DocDecision {
	d := doc
	d.Hosted, d.Owner = true, false
	d.Deleted = doc.Deleted || host.Deleted
	d.Archived = doc.Archived || host.Archived
	d.Was = min(host.Was, level.Edit)
	d.Level, d.Unlocked, d.Reasons = level.None, level.None, nil
	if d.Deleted {
		return d
	}
	d.Level = min(host.Level, level.Edit)
	d.Unlocked = d.Level
	if d.Level > level.None {
		d.Reasons = []level.Reason{level.Host}
	}
	return d
}

// HostPlace is where a hosted doc is, as its host says it to someone who may
// open the host: the sentence a docs path that refuses the doc answers with
// ("This page belongs to a row of Deals. Change it there."), and the address
// of the host's own page for it.
type HostPlace struct {
	Belongs string
	Path    string
}

// HostKind is what a kind of host tells docs about its docs.
type HostKind struct {
	// Place is where the host's doc is.
	Place func(ctx context.Context, hostUUID string) (HostPlace, error)
}

var (
	hostKindsMu sync.RWMutex
	hostKinds   = map[string]HostKind{}
)

// RegisterHostKind adds a kind of host, once, from its package's init: only
// a registered kind's docs can be made (EnsureHostedDoc).
func RegisterHostKind(kind string, k HostKind) {
	if kind == "" || k.Place == nil {
		panic(fmt.Sprintf("doc: no host kind can be registered as %q", kind))
	}
	hostKindsMu.Lock()
	defer hostKindsMu.Unlock()
	if _, taken := hostKinds[kind]; taken {
		panic(fmt.Sprintf("doc: the host kind %q is registered twice", kind))
	}
	hostKinds[kind] = k
}

func hostKindOf(kind string) (HostKind, bool) {
	hostKindsMu.RLock()
	defer hostKindsMu.RUnlock()
	k, ok := hostKinds[kind]
	return k, ok
}

// ErrNoHost is a host a doc can't be made for or placed: a kind nobody
// registered, or an id that isn't one.
var ErrNoHost = errors.New("doc: no such host")

// HostPlaceOf is where a host's doc is, as the host's kind says it. Ask it
// only for someone the host lets view it: the place names the host.
func HostPlaceOf(ctx context.Context, h Host) (HostPlace, error) {
	k, ok := hostKindOf(h.Kind)
	if !ok {
		return HostPlace{}, ErrNoHost
	}
	return k.Place(ctx, h.UUID)
}

// EnsureHostedDoc is the doc a host has, made the first time it is asked
// for, titled title: one doc a host, however many ask at once. made says
// whether this call made it. The caller has decided the person may (a row's
// page asks edit on the row). A doc made here has no maker, owner, shares,
// space or parent, and is private: no docs list or index ever holds it.
func EnsureHostedDoc(ctx context.Context, h Host, title string) (docUUID string, made bool, err error) {
	if _, ok := hostKindOf(h.Kind); !ok {
		return "", false, ErrNoHost
	}
	if _, err := uuid.Parse(h.UUID); err != nil {
		return "", false, ErrNoHost
	}
	id, made, err := domain.EnsureHostedDoc(ctx, h.Kind, strings.ToLower(h.UUID), uuid.NewString(), title, time.Now())
	if errors.Is(err, domain.ErrNoHost) {
		return "", false, ErrNoHost
	}
	return id, made, err
}

// HostedDocOf is a host's doc as the host's own read of it serves it: its
// title and body, and only when the doc is that host's (ErrNotFound for one
// that isn't, or isn't there). The caller has decided the person may open the
// host (a row's page asks the row).
func HostedDocOf(ctx context.Context, h Host, docUUID string) (*dgraphStruct.DgraphDoc, error) {
	doc, err := domain.GetSystemDocByUUID(ctx, docUUID)
	if err != nil {
		return nil, err
	}
	if doc == nil || doc.HostKind == "" || doc.HostKind != h.Kind || !strings.EqualFold(doc.HostUUID, h.UUID) {
		return nil, ErrNotFound
	}
	return doc, nil
}

// HostsChanged does what follows a change to who may open some hosts (a
// table made private, a row put in the trash or brought back): the live
// sockets on their docs are closed, everyone's, so that each reconnects and
// is authorised afresh by its host (I10). In the background; nothing here
// fails the change. A hosted doc's MQTT topic needs nothing: nobody can
// subscribe to one (ReadsDoc does not open hosted docs).
func HostsChanged(docUUIDs []string) {
	if len(docUUIDs) == 0 {
		return
	}
	ReauthLater(docUUIDs, nil)
}
