package domain

// The doc tree in the graph: a doc's place (doc_parent, doc_position), the
// levels the sidebar and the docs home read, the trash's batches, and what's
// gone for good. The tree's own rules (positions, loops, the lock) are
// business/Tree's; these are the doc's reads and writes.
//
// Every read here carries, for each doc, what the access rule needs to decide
// for the person asking ($viewer, their graph uid): its privacy, its creator
// and owner, and how it is shared with them. A title is shown only when that
// rule says yes (business/Doc), so a query here may name a doc the person
// can't open; nothing here decides who sees what.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/business/Tree/graph"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Doc"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"
)

// LiveDocFilter is "not deleted" (liveDocFilter), for the doc tree's schema
// in business/Doc.
const LiveDocFilter = liveDocFilter

// archivedDocFilter is a doc the workspace's retention archived (it is also
// deleted: business/Archive).
const archivedDocFilter = `gt(doc_archived_at, "1970-01-01T00:00:00Z")`

// deletedDocFilter is a doc in the trash or archived.
const deletedDocFilter = `gt(doc_deleted_at, "1970-01-01T00:00:00Z")`

// internalSpaces are spaces whose docs belong to something else and are never
// listed as docs: a table row's body (C3) lives in one.
const notInternalFilter = `NOT eq(doc_space_kind, "table")`

// docAccessFields is what the access rule reads of a doc for the person
// asking (docAccess.go's facts, which need AccessPrelude in the query), with
// its owner and maker named, as a row shows them.
const docAccessFields = accessScalars + accessCounts + `
	doc_created_by { uid user_uuid user_name user_full_name user_profile_object_key }
	doc_owner { uid user_uuid user_name user_full_name user_profile_object_key }`

// docRowFields is a doc as a row of the tree or of a list shows it.
const docRowFields = `
	uid
	doc_uuid
	doc_title
	doc_icon
	doc_position
	doc_snippet
	doc_created_at
	doc_updated_at
	doc_deleted_batch` + docAccessFields + `
	doc_child_count: count(~doc_parent @filter(` + liveDocFilter + `))`

// docPathFields is up to depth pages above a doc, each with what the access
// rule needs (a page the person can't open is shown as one, unnamed).
func docPathFields(depth int) string {
	if depth <= 0 {
		return ""
	}
	return `
	doc_parent @filter(` + liveDocFilter + `) {
		uid
		doc_uuid
		doc_title
		doc_icon` + docAccessFields + docPathFields(depth-1) + `
	}`
}

// docListPathDepth is how many pages above a list row its path shows.
const docListPathDepth = 3

// quoteUUIDs writes ids for an eq(doc_uuid, [...]) list, leaving out any that
// aren't uuids, as uuids are written (lower case).
func quoteUUIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if parsed, err := uuid.Parse(id); err == nil {
			out = append(out, `"`+parsed.String()+`"`)
		}
	}
	return out
}

// GetDocWithChildren reads a live doc and its live children, each as a row,
// at most limit children. ErrNotFound for no live doc.
func GetDocWithChildren(ctx context.Context, parentUUID, viewerUID string, limit int) (*dgraphStruct.DgraphDoc, error) {
	q := fmt.Sprintf(`query kids($id: string, $viewer: string) {`+AccessPrelude("$viewer")+`
		docInfo(func: eq(doc_uuid, $id)) @filter(%s) {%s
			doc_children: ~doc_parent(first: %d) @filter(%s) {%s
			}
		}
	}`, liveDocFilter, docRowFields, limit, liveDocFilter, docRowFields)
	doc, err := dgraphModels.GetDgraphDocInfoByUUID(ctx, q, map[string]string{"$id": parentUUID, "$viewer": viewerUID})
	helpers.LogReadFailure(ctx, err, "domain/GetDocWithChildren doc "+parentUUID)
	return doc, err
}

// Sections of the tree's top level, as the sidebar shows them to someone.
const (
	SectionPrivate   = "private"   // their own private pages
	SectionShared    = "shared"    // private pages others shared with them
	SectionWorkspace = "workspace" // pages everyone in the workspace can open
	SectionSpace     = "space"     // the pages of one project's or team's space
)

// GetDocTreeTop reads the top level of a section for the person: live docs
// of the section with no parent, or whose parent they can't see (deleted, or
// private and not theirs or shared with them): a page shared with someone
// whose parent isn't sits at the top for them. Every one of them, as rows.
func GetDocTreeTop(ctx context.Context, section, viewerUID, spaceKind, spaceUUID string, limit int) ([]*dgraphStruct.DgraphDoc, error) {
	var cands string
	vars := map[string]string{"$viewer": viewerUID}
	switch section {
	case SectionPrivate:
		cands = `cands as var(func: uid(owned, made)) @filter(eq(doc_private, true) AND ` + liveDocFilter + ` AND ` + notInternalFilter + ` AND (uid(owned) OR NOT has(doc_owner)))`
	case SectionShared:
		cands = `cands as var(func: uid(reading, editing, commenting, full, teamReading, teamCommenting, teamEditing)) @filter(eq(doc_private, true) AND ` + liveDocFilter + ` AND ` + notInternalFilter + ` AND NOT uid(owned) AND NOT (uid(made) AND NOT has(doc_owner)))`
	case SectionWorkspace:
		cands = `cands as var(func: eq(doc_private, false)) @filter(` + liveDocFilter + ` AND ` + notInternalFilter + ` AND NOT has(doc_space_uuid))`
	case SectionSpace:
		cands = `cands as var(func: eq(doc_space_uuid, $suuid)) @filter(eq(doc_space_kind, $skind) AND ` + liveDocFilter + `)`
		vars["$skind"], vars["$suuid"] = spaceKind, spaceUUID
	default:
		return nil, fmt.Errorf("domain/GetDocTreeTop: no section %q", section)
	}
	q := fmt.Sprintf(`query top($viewer: string, $skind: string, $suuid: string) {`+AccessPrelude("$viewer")+`
		var(func: uid($viewer)) {
			owned as ~doc_owner
			made as ~doc_created_by
			reading as ~doc_reading_users
			editing as ~doc_editing_users
			commenting as ~doc_commenting_users
			full as ~doc_full_access_users
		}
		# Docs shared with a team the person is in (the prelude's teams).
		var(func: uid(accTm, accTa)) @filter(`+liveTeamFilter+`) {
			teamReading as ~doc_team_viewers
			teamCommenting as ~doc_team_commenters
			teamEditing as ~doc_team_editors
		}
		%s
		top as var(func: uid(cands)) @filter(NOT has(doc_parent))
		var(func: uid(cands)) @filter(has(doc_parent)) {
			hidden as doc_parent @filter(%s OR (eq(doc_private, true) AND NOT uid(owned, made, reading, editing, commenting, full, teamReading, teamCommenting, teamEditing)))
		}
		var(func: uid(hidden)) {
			orphans as ~doc_parent @filter(uid(cands))
		}
		docInfo(func: uid(top, orphans), first: %d) {%s
		}
	}`, cands, deletedDocFilter, limit, docRowFields)
	docs, err := dgraphModels.GetDgraphDocs(ctx, q, vars)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDocTreeTop err: %+v", err)
	}
	return docs, err
}

// GetDocRowsByUUID reads docs by uuid, as list rows with their owner and the
// pages above them, deleted ones included (the caller decides): the docs of a
// list kept elsewhere (Recent, Starred) and a place's resolution.
func GetDocRowsByUUID(ctx context.Context, docUUIDs []string, viewerUID string) ([]*dgraphStruct.DgraphDoc, error) {
	quoted := quoteUUIDs(docUUIDs)
	if len(quoted) == 0 {
		return nil, nil
	}
	q := `query rows($viewer: string) {` + AccessPrelude("$viewer") + `
		docInfo(func: eq(doc_uuid, [` + strings.Join(quoted, ", ") + `])) {` + docRowFields + docPathFields(docListPathDepth) + `
		}
	}`
	docs, err := dgraphModels.GetDgraphDocs(ctx, q, map[string]string{"$viewer": viewerUID})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDocRowsByUUID err: %+v", err)
	}
	return docs, err
}

// GetDocTrash reads the docs a person can see in their trash: deleted since
// a moment (not archived: the retention archive keeps its own list), theirs or
// ones they could edit, newest first, with who deleted each, the delete it
// went with, and its parent's part in that delete.
func GetDocTrash(ctx context.Context, viewerUID string, since time.Time, limit int) ([]*dgraphStruct.DgraphDoc, error) {
	q := fmt.Sprintf(`query trash($viewer: string, $since: string) {`+AccessPrelude("$viewer")+`
		var(func: uid($viewer)) {
			owned as ~doc_owner
			made as ~doc_created_by
			editing as ~doc_editing_users
			full as ~doc_full_access_users
		}
		docInfo(func: uid(owned, made, editing, full), orderdesc: doc_deleted_at, first: %d) @filter(gt(doc_deleted_at, $since) AND NOT %s) {%s
			doc_deleted_by { uid user_uuid user_name user_full_name user_profile_object_key }
			doc_parent { uid doc_uuid doc_deleted_at doc_deleted_batch }
		}
	}`, limit, archivedDocFilter, docRowFields)
	docs, err := dgraphModels.GetDgraphDocs(ctx, q, map[string]string{"$viewer": viewerUID, "$since": since.UTC().Format(time.RFC3339)})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDocTrash err: %+v", err)
	}
	return docs, err
}

// GetDocBatch reads the docs deleted together with one delete, as rows, with
// each one's parent: what a restore brings back.
func GetDocBatch(ctx context.Context, batch, viewerUID string) ([]*dgraphStruct.DgraphDoc, error) {
	q := fmt.Sprintf(`query batch($batch: string, $viewer: string) {`+AccessPrelude("$viewer")+`
		docInfo(func: eq(doc_deleted_batch, $batch)) @filter(%s AND NOT %s) {%s
			doc_parent { uid doc_uuid doc_deleted_at doc_deleted_batch }
		}
	}`, deletedDocFilter, archivedDocFilter, docRowFields)
	docs, err := dgraphModels.GetDgraphDocs(ctx, q, map[string]string{"$batch": batch, "$viewer": viewerUID})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDocBatch err: %+v", err)
	}
	return docs, err
}

// MoveUp is a page that stays when the page above it goes, to the trash or for
// good (business/Doc docTake.go): it moves under Parent, a graph uid ("" is the
// top), at Key ("" keeps the key it has).
type MoveUp struct {
	UID, Parent, Key string
}

// writeMovesUp adds the moves to a write: each page's new parent (or none, at
// the top) and key.
func writeMovesUp(set, del *strings.Builder, up []MoveUp) error {
	for _, m := range up {
		if !helpers.IsDgraphUID(m.UID) || (m.Parent != "" && !helpers.IsDgraphUID(m.Parent)) {
			return fmt.Errorf("domain: a page moving up names %q under %q, which aren't uids", m.UID, m.Parent)
		}
		if m.Parent != "" {
			fmt.Fprintf(set, "<%s> <doc_parent> <%s> .\n", m.UID, m.Parent)
		} else {
			fmt.Fprintf(del, "<%s> <doc_parent> * .\n", m.UID)
		}
		if m.Key != "" {
			key, _ := json.Marshal(m.Key)
			fmt.Fprintf(set, "<%s> <doc_position> %s .\n", m.UID, key)
		}
	}
	return nil
}

// TrashDocs puts a live doc and the docs under it (uids, read under the
// tree's lock) in the trash as one delete: the same deletion time, batch and
// deleter for all, so a restore brings them back together. The pages under
// them that stay (up: the ones the person can't open) move up in the same
// write, so no page is ever left under one in the trash. The doc must still
// be live when the write lands (ErrNotFound / ErrDocDeleted otherwise), and
// the tree's stamp is written, so a move racing it is run again after it.
func TrashDocs(ctx context.Context, rootUUID string, under []string, batch, byUID string, at time.Time, up ...MoveUp) error {
	if _, err := uuid.Parse(rootUUID); err != nil {
		return ErrNoDocToUpdate
	}
	if !helpers.IsDgraphUID(byUID) {
		return fmt.Errorf("domain/TrashDocs: %q is not a uid", byUID)
	}
	atJSON, _ := json.Marshal(at.UTC().Format(time.RFC3339Nano))
	batchJSON, _ := json.Marshal(batch)
	var set strings.Builder
	write := func(subject string) {
		fmt.Fprintf(&set, "%s <doc_deleted_at> %s .\n%s <doc_deleted_batch> %s .\n%s <doc_deleted_by> <%s> .\n",
			subject, atJSON, subject, batchJSON, subject, byUID)
	}
	write("uid(root)")
	for _, u := range under {
		if !helpers.IsDgraphUID(u) {
			return fmt.Errorf("domain/TrashDocs: %q is not a uid", u)
		}
		write("<" + u + ">")
	}
	// Each doc's own uuid is written again, unchanged. Every save writes it
	// too (docUpdateRequest), so a save in flight when its doc goes to the
	// trash conflicts with this write, is run again, and finds the doc
	// deleted. Without it the save committed after the delete, and the edits
	// made after it landed on the trashed doc.
	fmt.Fprintf(&set, "uid(root) <doc_uuid> %q .\n", rootUUID)
	subtree := ""
	if len(under) > 0 {
		subtree = "\n\t\t\tsub as var(func: uid(" + strings.Join(under, ", ") + ")) { subUUID as doc_uuid }"
		set.WriteString("uid(sub) <doc_uuid> val(subUUID) .\n")
	}
	var del strings.Builder
	if err := writeMovesUp(&set, &del, up); err != nil {
		return err
	}
	query := graph.Stamped("doc", `query q($id: string) {
			root as var(func: eq(doc_uuid, $id)) @filter(`+liveDocFilter+`)
			found(func: eq(doc_uuid, $id)) { uid doc_deleted_at }`+subtree+`
		}`, &set, at)
	muts := []*api.Mutation{{SetNquads: []byte(set.String()), Cond: "@if(eq(len(root), 1))"}}
	if del.Len() > 0 {
		muts = append(muts, &api.Mutation{DelNquads: []byte(del.String()), Cond: "@if(eq(len(root), 1))"})
	}
	resp, err := dgraphInit.DoCommitNow(ctx, &api.Request{
		Query:     query,
		Vars:      map[string]string{"$id": rootUUID},
		Mutations: muts,
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/TrashDocs err: %+v", err)
		return err
	}
	return liveOrWhy(resp.GetJson())
}

// liveOrWhy reads the "found" block of an upsert on one doc: nil when it was
// live (and the write ran), ErrNotFound or ErrDocDeleted when not.
func liveOrWhy(raw []byte) error {
	var found struct {
		Found []*dgraphStruct.DgraphDoc `json:"found"`
	}
	if err := json.Unmarshal(raw, &found); err != nil {
		return err
	}
	switch {
	case len(found.Found) == 0:
		return ErrNotFound
	case helpers.IsSoftDeleted(found.Found[0].DeletedAt):
		return ErrDocDeleted
	}
	return nil
}

// RestoreDocs brings deleted docs (uids) back to live: the zero deletion
// time, no batch, deleter or archive mark; and the ones in toTop lose their
// parent (it is gone), landing at the top. Only docs still deleted are
// changed. Answers the uuids it restored.
func RestoreDocs(ctx context.Context, uids, toTop []string, at time.Time) ([]string, error) {
	var ok []string
	for _, u := range uids {
		if helpers.IsDgraphUID(u) {
			ok = append(ok, u)
		}
	}
	if len(ok) == 0 {
		return nil, nil
	}
	var del strings.Builder
	del.WriteString("uid(back) <doc_deleted_batch> * .\nuid(back) <doc_deleted_by> * .\nuid(back) <doc_archived_at> * .\n")
	for _, u := range toTop {
		if helpers.IsDgraphUID(u) {
			fmt.Fprintf(&del, "<%s> <doc_parent> * .\n", u)
		}
	}
	var set strings.Builder
	set.WriteString(`uid(back) <doc_deleted_at> "0001-01-01T00:00:00Z" .` + "\n")
	query := graph.Stamped("doc", `query {
			back as var(func: uid(`+strings.Join(ok, ", ")+`)) @filter(`+deletedDocFilter+`)
			restored(func: uid(back)) { doc_uuid }
		}`, &set, at)
	resp, err := dgraphInit.DoCommitNow(ctx, &api.Request{
		Query: query,
		Mutations: []*api.Mutation{
			{SetNquads: []byte(set.String()), Cond: "@if(gt(len(back), 0))"},
			{DelNquads: []byte(del.String()), Cond: "@if(gt(len(back), 0))"},
		},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/RestoreDocs err: %+v", err)
		return nil, err
	}
	var got struct {
		Restored []*dgraphStruct.DgraphDoc `json:"restored"`
	}
	if err := json.Unmarshal(resp.GetJson(), &got); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(got.Restored))
	for _, d := range got.Restored {
		out = append(out, d.Uuid)
	}
	return out, nil
}

// GetArchivedSubtree reads an archived doc and the archived docs under it,
// as rows with their parents: what bringing it back from the archive restores.
func GetArchivedSubtree(ctx context.Context, docUUID, viewerUID string) ([]*dgraphStruct.DgraphDoc, error) {
	q := fmt.Sprintf(`query sub($id: string, $viewer: string) {`+AccessPrelude("$viewer")+`
		root as var(func: eq(doc_uuid, $id)) @filter(%s)
		var(func: uid(root)) @recurse(depth: 34, loop: false) {
			under as ~doc_parent @filter(%s)
		}
		docInfo(func: uid(root, under)) {%s
			doc_parent { uid doc_uuid doc_deleted_at }
		}
	}`, archivedDocFilter, archivedDocFilter, docRowFields)
	docs, err := dgraphModels.GetDgraphDocs(ctx, q, map[string]string{"$id": docUUID, "$viewer": viewerUID})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetArchivedSubtree err: %+v", err)
	}
	return docs, err
}

// GetDocTreeLinks reads, for docs (by uuid), each one's live parent and live
// children: what the retention archive needs to take whole subtrees only.
func GetDocTreeLinks(ctx context.Context, docUUIDs []string) ([]*dgraphStruct.DgraphDoc, error) {
	quoted := quoteUUIDs(docUUIDs)
	if len(quoted) == 0 {
		return nil, nil
	}
	q := fmt.Sprintf(`{
		docInfo(func: eq(doc_uuid, [%s])) {
			doc_uuid
			doc_parent @filter(%s) { doc_uuid }
			doc_children: ~doc_parent @filter(%s) { doc_uuid }
		}
	}`, strings.Join(quoted, ", "), liveDocFilter, liveDocFilter)
	docs, err := dgraphModels.GetDgraphDocs(ctx, q, map[string]string{})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDocTreeLinks err: %+v", err)
	}
	return docs, err
}

// GetDocsTrashedBefore is up to limit docs in the trash since before a
// moment (not archived ones: the archive has its own retention), with their
// comments: what the trash's retention removes for good.
func GetDocsTrashedBefore(ctx context.Context, before time.Time, limit int) ([]*dgraphStruct.DgraphDoc, error) {
	q := fmt.Sprintf(`query due($before: string) {
		docInfo(func: has(doc_deleted_batch), first: %d) @filter(lt(doc_deleted_at, $before) AND %s AND NOT %s) {
			uid
			doc_uuid
			doc_comments { uid }
		}
	}`, limit, deletedDocFilter, archivedDocFilter)
	docs, err := dgraphModels.GetDgraphDocs(ctx, q, map[string]string{"$before": before.UTC().Format(time.RFC3339)})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDocsTrashedBefore err: %+v", err)
	}
	return docs, err
}

// PurgeDocNodes removes docs (uids) from the graph for good, with their
// comments and every edge others hold to them: a task's or project's link, a
// doc's mention, and a child's parent. The pages a person's delete for good
// leaves (business/Doc docTake.go) move up as up says, and those still in the
// trash become deletes of their own (rebatch, uid -> batch), in the same
// write; any other child of a removed doc lands at the top. Only docs still
// deleted are removed.
func PurgeDocNodes(ctx context.Context, docUIDs []string, commentUIDs []string, at time.Time, up []MoveUp, rebatch map[string]string) error {
	var ok []string
	for _, u := range docUIDs {
		if helpers.IsDgraphUID(u) {
			ok = append(ok, u)
		}
	}
	if len(ok) == 0 {
		return nil
	}
	var del, set strings.Builder
	del.WriteString("uid(gone) * * .\n")
	del.WriteString("uid(linkers) <linked_docs> uid(gone) .\n")
	del.WriteString("uid(mentioners) <doc_mentions> uid(gone) .\n")
	del.WriteString("uid(kids) <doc_parent> * .\n")
	for _, c := range commentUIDs {
		if helpers.IsDgraphUID(c) {
			fmt.Fprintf(&del, "<%s> * * .\n", c)
		}
	}
	if err := writeMovesUp(&set, &del, up); err != nil {
		return err
	}
	moving := make([]string, 0, len(up))
	for _, m := range up {
		moving = append(moving, m.UID)
	}
	staying := ""
	if len(moving) > 0 {
		staying = " AND NOT uid(" + strings.Join(moving, ", ") + ")"
	}
	uids := make([]string, 0, len(rebatch))
	for u := range rebatch {
		uids = append(uids, u)
	}
	sort.Strings(uids)
	for _, u := range uids {
		if !helpers.IsDgraphUID(u) {
			return fmt.Errorf("domain/PurgeDocNodes: %q is not a uid", u)
		}
		b, _ := json.Marshal(rebatch[u])
		fmt.Fprintf(&set, "<%s> <doc_deleted_batch> %s .\n", u, b)
	}
	query := graph.Stamped("doc", `query {
			gone as var(func: uid(`+strings.Join(ok, ", ")+`)) @filter(`+deletedDocFilter+`)
			var(func: uid(gone)) {
				linkers as ~linked_docs
				mentioners as ~doc_mentions
				kids as ~doc_parent @filter(NOT uid(gone)`+staying+`)
			}
		}`, &set, at)
	_, err := dgraphInit.DoCommitNow(ctx, &api.Request{
		Query: query,
		Mutations: []*api.Mutation{
			{DelNquads: []byte(del.String()), Cond: "@if(gt(len(gone), 0))"},
			{SetNquads: []byte(set.String()), Cond: "@if(gt(len(gone), 0))"},
		},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/PurgeDocNodes err: %+v", err)
	}
	return err
}

// CountDocBatches says how many docs each delete (batch) put in the trash.
func CountDocBatches(ctx context.Context, batches []string) (map[string]int, error) {
	out := map[string]int{}
	if len(batches) == 0 {
		return out, nil
	}
	quoted := make([]string, 0, len(batches))
	for _, b := range batches {
		if parsed, err := uuid.Parse(b); err == nil {
			quoted = append(quoted, `"`+parsed.String()+`"`)
		}
	}
	if len(quoted) == 0 {
		return out, nil
	}
	docs, err := dgraphModels.GetDgraphDocs(ctx, `{
		docInfo(func: eq(doc_deleted_batch, [`+strings.Join(quoted, ", ")+`])) @filter(`+deletedDocFilter+`) {
			doc_deleted_batch
		}
	}`, map[string]string{})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/CountDocBatches err: %+v", err)
		return nil, err
	}
	for _, d := range docs {
		out[d.DeletedBatch]++
	}
	return out, nil
}

// GetDocDeletedWithParent reads one deleted doc as a row, with its parent's
// deletion: a doc deleted before batches existed comes back alone.
func GetDocDeletedWithParent(ctx context.Context, docUUID, viewerUID string) ([]*dgraphStruct.DgraphDoc, error) {
	q := fmt.Sprintf(`query one($id: string, $viewer: string) {`+AccessPrelude("$viewer")+`
		docInfo(func: eq(doc_uuid, $id)) @filter(%s) {%s
			doc_parent { uid doc_uuid doc_deleted_at }
		}
	}`, deletedDocFilter, docRowFields)
	docs, err := dgraphModels.GetDgraphDocs(ctx, q, map[string]string{"$id": docUUID, "$viewer": viewerUID})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDocDeletedWithParent err: %+v", err)
	}
	return docs, err
}

// GetDocCommentUIDs is the graph uids of the docs' comments, deleted ones
// included: what goes with them when they're removed for good.
func GetDocCommentUIDs(ctx context.Context, docUIDs []string) ([]string, error) {
	var ok []string
	for _, u := range docUIDs {
		if helpers.IsDgraphUID(u) {
			ok = append(ok, u)
		}
	}
	if len(ok) == 0 {
		return nil, nil
	}
	docs, err := dgraphModels.GetDgraphDocs(ctx, `{
		docInfo(func: uid(`+strings.Join(ok, ", ")+`)) {
			uid
			doc_comments { uid }
		}
	}`, map[string]string{})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDocCommentUIDs err: %+v", err)
		return nil, err
	}
	var out []string
	for _, d := range docs {
		for _, c := range d.Comments {
			if c != nil && c.Uid != "" {
				out = append(out, c.Uid)
			}
		}
	}
	return out, nil
}

// NewDocNode is one doc to make: its fields, and where in the tree it goes:
// under the doc with ParentUUID (made only while that doc is live), or under
// another doc made in the same call (ParentRef, the index of an earlier one),
// or at the top.
type NewDocNode struct {
	Doc        *dgraphStruct.DgraphDoc
	ParentUUID string
	ParentRef  int // -1: none
}

// CreateDocNodes makes docs in one transaction: each under its parent, the
// first of them (and only it) under a live doc named by uuid, which must still
// be live when the write lands (ErrNotFound otherwise); siblings given fresh
// keys (renumber, by uuid) in the same transaction. Answers the new docs' uids.
func CreateDocNodes(ctx context.Context, nodes []NewDocNode, renumber map[string]string, at time.Time) ([]string, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	type obj map[string]any
	var set []obj
	parentUUID := ""
	for i, n := range nodes {
		raw, err := json.Marshal(n.Doc)
		if err != nil {
			return nil, err
		}
		var m obj
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		m["uid"] = fmt.Sprintf("_:n%d", i)
		switch {
		case n.ParentRef >= 0 && n.ParentRef < i:
			m["doc_parent"] = obj{"uid": fmt.Sprintf("_:n%d", n.ParentRef)}
		case n.ParentUUID != "" && i == 0:
			if _, err := uuid.Parse(n.ParentUUID); err != nil {
				return nil, ErrNoDocToUpdate
			}
			parentUUID = n.ParentUUID
			m["doc_parent"] = obj{"uid": "uid(parent)"}
		}
		set = append(set, m)
	}
	setJSON, err := json.Marshal(set)
	if err != nil {
		return nil, err
	}
	query := "query q($parent: string) {\n"
	cond := ""
	if parentUUID != "" {
		query += "\t\tparent as var(func: eq(doc_uuid, $parent)) @filter(" + liveDocFilter + ")\n"
		cond = "@if(eq(len(parent), 1))"
	}
	muts := []*api.Mutation{{SetJson: setJSON, Cond: cond}}
	i := 0
	for id, key := range renumber {
		parsed, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		keyJSON, _ := json.Marshal(key)
		query += fmt.Sprintf("\t\tsib%d as var(func: eq(doc_uuid, %q))\n", i, parsed.String())
		c := fmt.Sprintf("@if(eq(len(sib%d), 1))", i)
		if cond != "" {
			c = fmt.Sprintf("@if(eq(len(parent), 1) AND eq(len(sib%d), 1))", i)
		}
		muts = append(muts, &api.Mutation{SetNquads: []byte(fmt.Sprintf("uid(sib%d) <doc_position> %s .\n", i, keyJSON)), Cond: c})
		i++
	}
	if parentUUID != "" {
		query += "\t\tp(func: uid(parent)) { uid }\n"
	}
	query += "\t}"
	req := &api.Request{Query: query, Vars: map[string]string{"$parent": parentUUID}, Mutations: muts}
	if parentUUID == "" && len(renumber) == 0 {
		req.Query, req.Vars = "", nil
	}
	resp, err := dgraphInit.DoCommitNow(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/CreateDocNodes err: %+v", err)
		return nil, err
	}
	if parentUUID != "" {
		var found struct {
			P []struct{ UID string } `json:"p"`
		}
		if err := json.Unmarshal(resp.GetJson(), &found); err != nil {
			return nil, err
		}
		if len(found.P) != 1 {
			return nil, ErrNotFound
		}
	}
	uids := make([]string, len(nodes))
	for i := range nodes {
		uids[i] = resp.GetUids()[fmt.Sprintf("n%d", i)]
	}
	return uids, nil
}

// GetDocsForCopy reads docs by uuid with everything a copy takes from them:
// title, body, look and style, and where each sits (its parent's uuid and
// its position), as rows for the person asking.
func GetDocsForCopy(ctx context.Context, docUUIDs []string, viewerUID string) ([]*dgraphStruct.DgraphDoc, error) {
	quoted := quoteUUIDs(docUUIDs)
	if len(quoted) == 0 {
		return nil, nil
	}
	q := `query copy($viewer: string) {` + AccessPrelude("$viewer") + `
		docInfo(func: eq(doc_uuid, [` + strings.Join(quoted, ", ") + `])) @filter(` + liveDocFilter + `) {` + docRowFields + `
			doc_body
			doc_cover_key
			doc_cover_offset
			doc_full_width
			doc_font
			doc_small_text
			doc_parent { uid doc_uuid }
		}
	}`
	docs, err := dgraphModels.GetDgraphDocs(ctx, q, map[string]string{"$viewer": viewerUID})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDocsForCopy err: %+v", err)
	}
	return docs, err
}
