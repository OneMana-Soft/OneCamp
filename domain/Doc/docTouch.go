package domain

// A doc's last change that isn't its body (docs F1): a file's new version.
// It is recorded as every change is, when (doc_updated_at) and by whom
// (doc_updated_by), both in one write, so a row's "Maya · 3h ago" names the
// person behind its time. A colour is not a change (docColor.go writes
// neither).

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
	"github.com/google/uuid"
)

// ErrNoEditor is a change credited to someone who isn't a person's uuid.
var ErrNoEditor = errors.New("doc: no one to name as the last editor")

// touchQuery finds the live doc by its uuid, and the editor by theirs.
var touchQuery = `query q($id: string, $editor: string) {
	doc as var(func: eq(doc_uuid, $id)) @filter(` + liveDocFilter + `) { u as doc_uuid }
	editor as var(func: ` + dgraphquery.PersonByUUID("$editor") + `)
}`

// touchRequest is TouchDoc's upsert: one mutation that sets when the doc last
// changed and who changed it, on a live doc and an editor in the graph, both
// or neither (an edge to an empty variable would make a node); and the doc's
// uuid written again, as every doc update writes it, so a delete committed
// while this is in flight aborts it. Pure.
func touchRequest(docUUID, editorUUID string, at time.Time) (string, map[string]string, dgexec.Mutation, error) {
	doc, err := uuid.Parse(docUUID)
	if err != nil {
		return "", nil, dgexec.Mutation{}, ErrNoDocToUpdate
	}
	editor, err := uuid.Parse(editorUUID)
	if err != nil {
		return "", nil, dgexec.Mutation{}, ErrNoEditor
	}
	if at.IsZero() {
		at = time.Now()
	}
	when, _ := json.Marshal(at.UTC().Format(time.RFC3339Nano))
	m := dgexec.Mutation{
		SetNQuads: "uid(doc) <doc_uuid> val(u) .\n" +
			"uid(doc) <doc_updated_at> " + string(when) + " .\n" +
			"uid(doc) <doc_updated_by> uid(editor) .\n",
		Cond: "@if(eq(len(doc), 1) AND eq(len(editor), 1))",
	}
	return touchQuery, map[string]string{"$id": doc.String(), "$editor": editor.String()}, m, nil
}

// TouchDoc records a change to a doc that isn't its body (a file's new
// version): when it was made (at; now for zero) and by whom (the editor's
// user uuid, the server's account, never a client's word), in one write.
// Nothing is written to a doc that isn't there or is in the trash.
func TouchDoc(ctx context.Context, docUUID, editorUUID string, at time.Time) error {
	q, vars, m, err := touchRequest(docUUID, editorUUID, at)
	if err != nil {
		return err
	}
	if _, err := dgexec.Mutate(ctx, q, vars, m); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/TouchDoc doc %s err: %+v", docUUID, err)
		return err
	}
	return nil
}
