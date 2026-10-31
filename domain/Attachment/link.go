package Domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// fileHolders is what a file is linked to in the graph, by the holder's kind:
// the predicate that names a holder, and the one that lists its files.
var fileHolders = map[string]struct{ key, files string }{
	"task":    {"task_uuid", "task_attachments"},
	"comment": {"comment_uuid", "comment_attachments"},
	"post":    {"post_uuid", "post_attachments"},
	"chat":    {"chat_uuid", "chat_attachments"},
}

// ErrNoHolder is a file's link to a holder the graph doesn't have as one
// node.
var ErrNoHolder = errors.New("the file's holder isn't one node in the graph")

// LinkFile writes a file's node, the one with its uuid or else a new one, and
// links it to its holder (kind "task", "comment", "post" or "chat", by uuid),
// in one upsert:
// a retry writes the same node and link again, and the holder lists the file
// once. When the graph doesn't have the holder as one node it writes nothing
// and answers ErrNoHolder.
func LinkFile(ctx context.Context, kind, holder string, file *dgraphStruct.DgraphAttachment) error {
	h, ok := fileHolders[kind]
	if !ok {
		return fmt.Errorf("a file isn't linked to a %q", kind)
	}
	node := fileNode(file)
	res, err := dgexec.Mutate(ctx, `query Link($holder: string, $file: string) {
		holders(func: eq(`+h.key+`, $holder)) { holder as uid `+h.key+` }
		file as var(func: eq(attachment_uuid, $file))
	}`, map[string]string{"$holder": holder, "$file": file.Uuid}, dgexec.Mutation{
		Cond: "@if(eq(len(holder), 1))",
		Set:  []any{node, map[string]any{"uid": dgexec.Var("holder"), h.files: []map[string]string{{"uid": dgexec.Var("file")}}}},
	})
	if err != nil {
		return err
	}
	var found struct {
		Holders []json.RawMessage `json:"holders"`
	}
	if err := json.Unmarshal(res.GetJson(), &found); err != nil {
		return fmt.Errorf("read the link's holder: %w", err)
	}
	if len(found.Holders) != 1 {
		return ErrNoHolder
	}
	return nil
}

// ListedFiles is which of files (uuids) each holder of a kind (by uuid)
// lists already, as holder -> file -> true.
func ListedFiles(ctx context.Context, kind string, holders, files []string) (map[string]map[string]bool, error) {
	h, ok := fileHolders[kind]
	if !ok {
		return nil, fmt.Errorf("a file isn't linked to a %q", kind)
	}
	holderList, fileList := dgraphquery.UUIDList(holders), dgraphquery.UUIDList(files)
	if holderList == "" || fileList == "" {
		return nil, nil
	}
	got, err := dgexec.Read[struct {
		Holders []struct {
			Holder string `json:"holder"`
			Files  []struct {
				Uuid string `json:"attachment_uuid"`
			} `json:"files"`
		} `json:"holders"`
	}](ctx, `query listed {
		holders(func: eq(`+h.key+`, [`+holderList+`])) {
			holder: `+h.key+`
			files: `+h.files+` @filter(eq(attachment_uuid, [`+fileList+`])) { attachment_uuid }
		}
	}`, nil)
	if err != nil {
		return nil, err
	}
	listed := make(map[string]map[string]bool, len(got.Holders))
	for _, holder := range got.Holders {
		for _, f := range holder.Files {
			if listed[holder.Holder] == nil {
				listed[holder.Holder] = map[string]bool{}
			}
			listed[holder.Holder][f.Uuid] = true
		}
	}
	return listed, nil
}

// WriteFileNode writes a file's node alone, the one with its uuid or else a
// new one (fileNode), for a file no holder in the graph lists: a project's
// (business/Import linkKeptFile). A retry writes the same node again.
func WriteFileNode(ctx context.Context, file *dgraphStruct.DgraphAttachment) error {
	_, err := dgexec.Mutate(ctx, `query File($file: string) { file as var(func: eq(attachment_uuid, $file)) }`,
		map[string]string{"$file": file.Uuid}, dgexec.Mutation{Set: fileNode(file)})
	return err
}

// fileNode is a file's node as an upsert by its uuid writes it (the variable
// file): the node the graph holds, or else a new one. It always carries a
// time, its maker's (an import's file stage sets it) or else the write's, so
// no write makes a file's node with none (helpers
// TestEveryAttachmentNodeTheServerWritesSaysWhenItWasWritten).
func fileNode(file *dgraphStruct.DgraphAttachment) *dgraphStruct.DgraphAttachment {
	node := *file
	at := time.Now()
	if file.CreatedAt != nil {
		at = *file.CreatedAt
	}
	node.CreatedAt = &at
	node.Uid, node.DType = dgexec.Var("file"), []string{"Attachment"}
	return &node
}
