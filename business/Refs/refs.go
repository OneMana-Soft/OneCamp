// Package refsBusiness keeps the work graph's references (DG6): refers_to,
// from each message, direct or group message, comment and doc to the docs,
// tasks and projects its body links to, so a link written anywhere shows in
// the linked thing's Connected panel (business/WorkGraph) and its paths.
//
// HOW AN EDGE IS KEPT. A save of a body tells this package (Saved, from the
// business code that wrote it). That writes an outbox event (business/Outbox,
// migration 347) naming the source by kind and uuid, never its body, and
// releases and delivers it at once. The sink (Reconcile) reads the source by
// its uuid as it is when it runs, parses its body (helpers/bodyrefs, the
// chips' parser) and makes refers_to exactly the set the body links to: adds
// and removes, in one upsert, each target found by its uuid in that upsert.
// Running it twice changes nothing the second time, so the outbox's "at least
// once" is harmless, and an event for something deleted since does what the
// body says or nothing at all.
//
// A save whose event couldn't be written, or a writer this package doesn't
// hear from (an import, a restore), is caught by the nightly reconciler
// (reconcile.go), which re-derives the edges from the bodies in bounded pages
// and reports and fixes the drift; `go-one-camp workgraphrefs --apply`
// rebuilds every edge after a restore.
//
// WHAT A SOURCE LINKS TO.
//   - A message, direct message or comment: every doc, task and project its
//     body links to, by a chip, a link or a link written out in text.
//   - A doc: the tasks and projects its body links to with a chip or a link
//     (a doc's links to docs are doc_mentions, kept on save by business/Doc
//     and walked by the work graph already), and the tasks it holds a to-do
//     for (doc_task_links, migration 340). A to-do's data-task-id alone is no
//     link: only the conversion that checked the person's rights makes one.
//   - Never itself.
//
// WHO SEES AN EDGE. No one by its existence: every node the work graph reads
// is decided as the reader (business/EntityLink ReadMany), and a path through
// anything hidden is dropped whole. An edge to something that doesn't exist
// is never written (its uuid finds no node).
package refsBusiness

import (
	"context"
	"strings"

	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	domain "github.com/akashc777/OneCamp/domain/WorkGraph"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/bodyrefs"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	linkModels "github.com/akashc777/OneCamp/models/postgres/EntityLink"
	"github.com/google/uuid"
)

// The outbox topic and its one sink.
const (
	Topic    = "workgraph.refs"
	SinkName = "graph.refers_to"
	// KindChanged is the event's kind: a source's body (or a doc's to-dos)
	// changed.
	KindChanged = "refs.changed"
)

// The kinds a body is kept for, as the work graph names them.
const (
	Message = domain.KindMessage
	Chat    = domain.KindChat
	Comment = domain.KindComment
	Doc     = domain.KindDoc
)

// Seams for tests: the outbox, the graph and doc_task_links.
var (
	enqueueReady = func(ctx context.Context, e outboxBusiness.Entry) error {
		if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
			return errNoOutbox
		}
		id, err := outboxBusiness.Add(ctx, postgresInit.DBConn.SqlDB, e)
		if err != nil {
			return err
		}
		if err := outboxBusiness.Release(ctx, id); err != nil {
			// Held, it is released by the drain once its time is up: the
			// topic has no resolver, because the body was saved before this.
			return nil
		}
		outboxBusiness.Deliver(id)
		return nil
	}
	readSource   = domain.ReadSource
	writeFixes   = domain.WriteFixes
	tasksForDocs = linkModels.TasksForDocs
)

type outboxError string

func (e outboxError) Error() string { return string(e) }

const errNoOutbox = outboxError("refs: no outbox (Postgres isn't connected)")

func init() {
	outboxBusiness.Sinks.Register(outboxBusiness.Sink{Topic: Topic, Name: SinkName, Run: runSink})
}

// Saved tells the work graph that a source's body was written: created is a
// new one, whose event is skipped when its body links to nothing (it has no
// edge to take off); an edit always goes, since it may have taken a link out.
// It never fails the save: an event that couldn't be written is the nightly
// reconciler's.
func Saved(ctx context.Context, kind domain.Kind, id, body string, created bool) {
	if !domain.IsRefSource(kind) || !helpers.IsCanonicalUUID(id) {
		return
	}
	if created && len(bodyLinks(kind, id, body)) == 0 {
		return
	}
	Changed(ctx, kind, id)
}

// Changed writes the event that keeps a source's edges, released and
// delivered at once: two Postgres statements on the caller's path, the rest
// on the outbox's.
func Changed(ctx context.Context, kind domain.Kind, id string) {
	if !domain.IsRefSource(kind) || !helpers.IsCanonicalUUID(id) {
		return
	}
	id = strings.ToLower(id)
	err := enqueueReady(ctx, outboxBusiness.Entry{
		Topic: Topic, Kind: KindChanged, Key: "refs:" + string(kind) + ":" + id,
		Entities: map[string]string{"kind": string(kind), "uuid": id},
		Sinks:    []string{SinkName},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Refs %s %s: its links' event wasn't written, so tonight's reconcile keeps them: %v", kind, id, err)
	}
}

// runSink is the outbox's sink: Reconcile the source the event names. An
// event naming no source it knows is done (nothing to do).
func runSink(ctx context.Context, ev outboxBusiness.Event) error {
	kind := domain.Kind(ev.Entities["kind"])
	id := ev.Entities["uuid"]
	if !domain.IsRefSource(kind) || !helpers.IsCanonicalUUID(id) {
		return nil
	}
	_, err := Reconcile(ctx, kind, id)
	return err
}

// Reconcile makes one source's refers_to exactly what its body (and, for a
// doc, its to-dos) links to, and answers the fix it applied (empty when
// nothing changed, or the source is gone). Round trips: one graph read, at
// most one graph write, and for a doc one Postgres read.
func Reconcile(ctx context.Context, kind domain.Kind, id string) (domain.Fix, error) {
	src, err := readSource(ctx, kind, id)
	if err != nil || src == nil {
		return domain.Fix{}, err
	}
	var tasks []string
	if kind == Doc {
		got, err := docTasks(ctx, []string{src.UUID})
		if err != nil {
			return domain.Fix{}, err
		}
		tasks = got[src.UUID]
	}
	fix := domain.Diff(*src, Want(*src, tasks))
	if fix.Empty() {
		return fix, nil
	}
	return fix, writeFixes(ctx, []domain.Fix{fix})
}

// Want is what a source should link to: its body's links (bodyLinks) and,
// for a doc, the tasks it holds a to-do for. Pure.
func Want(s domain.Source, docTasks []string) []domain.Ref {
	out := bodyLinks(s.Kind, s.UUID, s.Body)
	seen := map[domain.Ref]bool{}
	for _, r := range out {
		seen[r] = true
	}
	for _, t := range docTasks {
		u, err := uuid.Parse(t)
		if err != nil {
			continue
		}
		r := domain.Ref{Kind: domain.KindTask, UUID: u.String()}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// bodyLinks is what a source's body links to, by the rules in the package
// comment. Pure.
func bodyLinks(kind domain.Kind, self, body string) []domain.Ref {
	if body == "" {
		return nil
	}
	self = strings.ToLower(self)
	var out []domain.Ref
	for _, l := range bodyrefs.Scan(body).Links {
		if kind == Doc && (l.FromText || l.Kind == bodyrefs.KindDoc) {
			continue
		}
		if l.UUID == self {
			continue
		}
		out = append(out, domain.Ref{Kind: domain.Kind(l.Kind), UUID: l.UUID})
	}
	return out
}

// docTasks is the tasks each doc (by lower-case uuid) holds a to-do for.
func docTasks(ctx context.Context, docs []string) (map[string][]string, error) {
	ids := make([]uuid.UUID, 0, len(docs))
	for _, d := range docs {
		if u, err := uuid.Parse(d); err == nil {
			ids = append(ids, u)
		}
	}
	got, err := tasksForDocs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(got))
	for d, ts := range got {
		for _, t := range ts {
			out[d.String()] = append(out[d.String()], t.String())
		}
	}
	return out, nil
}

// CommentSaved is Saved for a comment written in its parent's list (as the
// writers build it: the parent with the one comment): the comment with uuid
// id is found in comments, and its text is the body.
func CommentSaved(ctx context.Context, comments []*dgraphStruct.DgraphComment, id string, created bool) {
	if c := commentIn(comments, id); c != nil {
		Saved(ctx, Comment, id, c.Text, created)
	}
}

// commentIn is the comment with uuid id in comments, or nil. Pure.
func commentIn(comments []*dgraphStruct.DgraphComment, id string) *dgraphStruct.DgraphComment {
	for _, c := range comments {
		if c != nil && strings.EqualFold(c.Uuid, id) {
			return c
		}
	}
	return nil
}
