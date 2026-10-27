package Domain

// The reads and writes behind taking a deleted item's own files off with it.
// See business/Attachment ArchiveFilesOf for the rule; this file only knows
// the graph's edges and the attachments table.

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// A Holder is a kind of item that holds files of its own in the graph, by
// its edges: edges are the var blocks under the item, files names the vars
// that hold its own files and its thread's replies', and paired the vars that
// hold the files of the items the server paired with it, which show the same
// files ("" for none): a reply sent to its channel too is the reply and its
// copy there, linked by post_broadcast_of when the server posts the copy.
// table is the item's Postgres table and deleted its graph deletion time.
type Holder struct {
	id, edges, files, paired string
	table, deleted           string
}

// The items a person deletes with their files: a channel's post (its own
// replies' copies, and the reply it copies when it is a copy, are its
// pairs), a message of a conversation or a group, and a comment (on a post,
// a message, a task or a doc; a reply's copies are its pairs).
var (
	Post = Holder{id: "post_uuid", edges: `
			own as post_attachments
			post_comments {
				onReplies as comment_attachments
				~post_broadcast_of { onCopies as post_attachments }
			}
			post_broadcast_of { onCopied as comment_attachments }`,
		files: "own, onReplies", paired: "onCopies, onCopied", table: "posts", deleted: "post_deleted_at"}
	Chat = Holder{id: "chat_uuid", edges: `
			own as chat_attachments
			chat_comments { onReplies as comment_attachments }`,
		files: "own, onReplies", table: "chats", deleted: "chat_deleted_at"}
	Comment = Holder{id: "comment_uuid", edges: `
			own as comment_attachments
			~post_broadcast_of { onCopies as post_attachments }`,
		files: "own", paired: "onCopies", table: "comments", deleted: "comment_deleted_at"}
)

// Held is what ArchiveHeldFiles found: the ids of every file the item holds,
// and of those, the ones an item the server paired with it still shows.
type Held struct {
	Files  []uuid.UUID
	Paired map[uuid.UUID]bool
}

// ArchiveHeldFiles stamps the file nodes the item id holds, its own and its
// replies', deleted at at, in one upsert: by the item's own edges, never by
// a file's id, which a node of another item can share. It answers every file
// the item holds, those stamped before as well, so a run again after a failed
// one finishes it, and which of them a paired item still shows, read before
// the stamp in the same transaction.
func ArchiveHeldFiles(ctx context.Context, h Holder, id uuid.UUID, at time.Time) (Held, error) {
	paired := ""
	if h.paired != "" {
		paired = `paired(func: uid(` + h.paired + `)) @filter(` + dgraphquery.Live("attachment_deleted_at") + `) { attachment_uuid }`
	}
	res, err := dgexec.Mutate(ctx, `query Held($id: string) {
		var(func: eq(`+h.id+`, $id)) {`+h.edges+`
		}
		files(func: uid(`+h.files+`)) { attachment_uuid }
		live as var(func: uid(`+h.files+`)) @filter(`+dgraphquery.Live("attachment_deleted_at")+`)
		`+paired+`
	}`, map[string]string{"$id": id.String()}, dgexec.Mutation{
		Cond: "@if(gt(len(live), 0))",
		Set:  dgraphStruct.DgraphAttachment{Uid: dgexec.Var("live"), DeletedAt: &at},
	})
	if err != nil {
		return Held{}, err
	}
	type node struct {
		UUID string `json:"attachment_uuid"`
	}
	var out struct {
		Files  []node `json:"files"`
		Paired []node `json:"paired"`
	}
	if err := json.Unmarshal(res.GetJson(), &out); err != nil {
		return Held{}, err
	}
	held := Held{Files: make([]uuid.UUID, 0, len(out.Files)), Paired: map[uuid.UUID]bool{}}
	for _, f := range out.Files {
		if u, err := uuid.Parse(f.UUID); err == nil {
			held.Files = append(held.Files, u)
		}
	}
	for _, f := range out.Paired {
		if u, err := uuid.Parse(f.UUID); err == nil {
			held.Paired[u] = true
		}
	}
	return held, nil
}

// A HolderNode is a live node naming a file: its uid, when it was written
// (attachment_created_at; the zero time for a node with none, written before
// the server set one), and the ids of who added it (attachment_created_by: the
// server records the adder of a node it writes, models/dgraph NewAttachments).
type HolderNode struct {
	Uid uint64
	At  time.Time
	By  map[uuid.UUID]bool
}

// LiveHolders is the live nodes naming each of ids; an id no live node names
// is absent.
func LiveHolders(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]HolderNode, error) {
	list := dgraphquery.UUIDListOf(ids)
	if list == "" {
		return nil, nil
	}
	live := dgraphquery.Live("attachment_deleted_at")
	// Every live node answers in nodes, with its time if it has one; adders
	// answers a flat row per node and adder, whatever the edge's arity.
	read, err := dgexec.Read[struct {
		Nodes []struct {
			Node string     `json:"node"`
			File string     `json:"file"`
			At   *time.Time `json:"at"`
		} `json:"nodes"`
		Adders []struct {
			Node string `json:"node"`
			By   string `json:"by"`
		} `json:"adders"`
	}](ctx, `{
		nodes(func: eq(attachment_uuid, [`+list+`])) @filter(`+live+`) {
			node: uid
			file: attachment_uuid
			at: attachment_created_at
		}
		adders(func: eq(attachment_uuid, [`+list+`])) @filter(`+live+`) @normalize {
			node: uid
			attachment_created_by { by: user_uuid }
		}
	}`, nil)
	if err != nil {
		return nil, err
	}
	nodes := make(map[uint64]*HolderNode, len(read.Nodes))
	files := make(map[uint64]uuid.UUID, len(read.Nodes))
	for _, n := range read.Nodes {
		f, ferr := uuid.Parse(n.File)
		uid, uerr := parseUid(n.Node)
		if ferr != nil || uerr != nil {
			continue
		}
		h := &HolderNode{Uid: uid, By: map[uuid.UUID]bool{}}
		if n.At != nil {
			h.At = *n.At
		}
		nodes[uid], files[uid] = h, f
	}
	for _, a := range read.Adders {
		uid, uerr := parseUid(a.Node)
		by, berr := uuid.Parse(a.By)
		if h, seen := nodes[uid]; uerr == nil && berr == nil && seen {
			h.By[by] = true
		}
	}
	out := make(map[uuid.UUID][]HolderNode, len(nodes))
	for uid, h := range nodes {
		out[files[uid]] = append(out[files[uid]], *h)
	}
	return out, nil
}

// An ImportNode is a node of a file listed by the item an import recorded as
// the file's holder: its uid, its time, and whether it is stamped deleted.
type ImportNode struct {
	Uid     uint64
	At      time.Time
	Stamped bool
}

// importHolders are, by the kind of holder an import's file stage records
// (business/SlackImport file_worker: post or chat; business/Import
// attachment_worker: task, comment or project), the predicate naming the item
// and the edge to its files.
var importHolders = map[string][2]string{
	"post":    {"post_uuid", "post_attachments"},
	"chat":    {"chat_uuid", "chat_attachments"},
	"comment": {"comment_uuid", "comment_attachments"},
	"task":    {"task_uuid", "task_attachments"},
	"project": {"project_uuid", "project_attachments"},
}

// ImportNodes is, for each of ids an import made (an import's map of the
// files it made names it: import_id_map, entity "file", as
// models/postgres/Import EntityFile), the nodes of it that the items its
// imports recorded holding it list (the map's parent_kind and parent_uuid),
// stamped or not. An id no import made is absent; an imported one whose items
// list none of its nodes maps to an empty list.
func ImportNodes(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]ImportNode, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	type recorded struct {
		file, item uuid.UUID
		kind       string
	}
	recs, err := pgexec.Query(ctx, `SELECT onecamp_uuid, COALESCE(metadata->>'parent_kind', ''), COALESCE(metadata->>'parent_uuid', '')
		FROM import_id_map WHERE entity_type = 'file' AND onecamp_uuid = ANY($1::uuid[])`,
		func(r pgexec.Row) (recorded, error) {
			var x recorded
			var item string
			err := r.Scan(&x.file, &x.kind, &item)
			x.item, _ = uuid.Parse(item)
			return x, err
		}, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID][]ImportNode, len(recs))
	items := map[string][]uuid.UUID{}
	for _, x := range recs {
		if _, seen := out[x.file]; !seen {
			out[x.file] = []ImportNode{}
		}
		if _, ok := importHolders[x.kind]; ok && x.item != uuid.Nil {
			items[x.kind] = append(items[x.kind], x.item)
		}
	}
	if len(items) == 0 {
		return out, nil
	}
	// One read: each kind's items, with the nodes of these files they list.
	kinds := make([]string, 0, len(items))
	for kind := range items {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	files := dgraphquery.UUIDListOf(ids)
	q := "{"
	for _, kind := range kinds {
		e := importHolders[kind]
		q += `
		` + kind + `(func: eq(` + e[0] + `, [` + dgraphquery.UUIDListOf(items[kind]) + `])) {
			files: ` + e[1] + ` @filter(eq(attachment_uuid, [` + files + `])) {
				node: uid
				file: attachment_uuid
				at: attachment_created_at
				gone: attachment_deleted_at
			}
		}`
	}
	q += "\n\t}"
	read, err := dgexec.Read[map[string][]struct {
		Files []struct {
			Node string     `json:"node"`
			File string     `json:"file"`
			At   *time.Time `json:"at"`
			Gone *time.Time `json:"gone"`
		} `json:"files"`
	}](ctx, q, nil)
	if err != nil {
		return nil, err
	}
	epoch := time.Unix(0, 0)
	for _, kind := range kinds {
		for _, item := range read[kind] {
			for _, n := range item.Files {
				f, ferr := uuid.Parse(n.File)
				uid, uerr := parseUid(n.Node)
				if _, imported := out[f]; ferr != nil || uerr != nil || !imported {
					continue
				}
				in := ImportNode{Uid: uid, Stamped: n.Gone != nil && n.Gone.After(epoch)}
				if n.At != nil {
					in.At = *n.At
				}
				out[f] = append(out[f], in)
			}
		}
	}
	return out, nil
}

// parseUid is a graph uid ("0x1f") as a number: uids are given out in rising
// order, so of two nodes the one with the lower uid was written first.
func parseUid(s string) (uint64, error) {
	return strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
}

// A Row is what a file's attachments row, not stamped deleted, says of it:
// who uploaded it (uuid.Nil for no one) and when the server made it, at
// upload or at the import that made the file.
type Row struct {
	By uuid.UUID
	At time.Time
}

// Rows is the Row of each of ids whose row isn't stamped deleted; one stamped
// already is missing.
func Rows(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Row, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	type row struct {
		id uuid.UUID
		r  Row
	}
	rows, err := pgexec.Query(ctx, `SELECT id, created_by, created_at FROM attachments
		WHERE id = ANY($1::uuid[]) AND deleted_at IS NULL`,
		func(r pgexec.Row) (row, error) {
			var x row
			var by uuid.NullUUID
			var at sql.NullTime
			err := r.Scan(&x.id, &by, &at)
			x.r = Row{By: by.UUID, At: at.Time}
			return x, err
		}, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]Row, len(rows))
	for _, r := range rows {
		out[r.id] = r.r
	}
	return out, nil
}

// ArchiveRows stamps the attachments rows of ids deleted at at, those not
// stamped before, in one statement, and answers how many it stamped: every
// read by id then answers that the file isn't there, and the purge takes it
// once its policy's days have passed from at (ListAttachmentsArchivedBefore).
// A row keeps the time it was first stamped.
func ArchiveRows(ctx context.Context, ids []uuid.UUID, at time.Time) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	n, err := pgexec.Exec(ctx, `UPDATE attachments SET deleted_at = $1 WHERE id = ANY($2::uuid[]) AND deleted_at IS NULL`,
		at, pq.Array(ids))
	return int(n), err
}

// DeletedItems is up to n of h's items deleted in Postgres, by id, after
// after (uuid.Nil for the first).
func DeletedItems(ctx context.Context, h Holder, after uuid.UUID, n int) ([]uuid.UUID, error) {
	return pgexec.Query(ctx, `SELECT id FROM `+h.table+` WHERE deleted_at IS NOT NULL AND id > $1 ORDER BY id LIMIT $2`,
		func(r pgexec.Row) (uuid.UUID, error) {
			var id uuid.UUID
			err := r.Scan(&id)
			return id, err
		}, after, n)
}

// DeletedInGraph is which of ids, h's items, the graph holds deleted.
func DeletedInGraph(ctx context.Context, h Holder, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	list := dgraphquery.UUIDListOf(ids)
	if list == "" {
		return nil, nil
	}
	rows, err := dgexec.List[struct {
		ID string `json:"id"`
	}](ctx, `{
		gone(func: eq(`+h.id+`, [`+list+`])) @filter(`+dgraphquery.Deleted(h.deleted)+`) { id: `+h.id+` }
	}`, nil, "gone")
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]bool, len(rows))
	for _, r := range rows {
		if id, err := uuid.Parse(r.ID); err == nil {
			out[id] = true
		}
	}
	return out, nil
}
