package business

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	auditModel "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

const recDoc = "0b9b2c1e-3f4a-4d5e-8f60-718293a4b5c6"

// recorded is one row a test saw written.
type recorded struct {
	kind, action, summary string
	meta                  map[string]interface{}
}

// fakeRecord swaps the record for a list; the returned function puts it back.
func fakeRecord(t *testing.T) *[]recorded {
	t.Helper()
	saved := recordEntry
	t.Cleanup(func() { recordEntry = saved })
	var rows []recorded
	recordEntry = func(_ context.Context, _ *uuid.UUID, _ string, kind, action, summary string, meta map[string]interface{}) error {
		rows = append(rows, recorded{kind, action, summary, meta})
		return nil
	}
	return &rows
}

func fakeDecision(t *testing.T, d accessBusiness.Decision, err error) {
	t.Helper()
	saved := resolveDoc
	t.Cleanup(func() { resolveDoc = saved })
	resolveDoc = func(context.Context, accessBusiness.Subject, string) (accessBusiness.Decision, error) { return d, err }
}

func member() *userModels.UserInfo {
	u := &userModels.UserInfo{}
	u.UserPostgresInfo.Id = uuid.MustParse("00000000-0000-4000-8000-000000000001")
	u.UserPostgresInfo.EmailID = "sam@example.com"
	u.UserDgraphInfo.Uid = "0x1"
	u.UserDgraphInfo.Uuid = "11111111-1111-4111-8111-111111111111"
	return u
}

// A doc the person can't open is not found, whatever the reason, and the
// refusal is recorded; one they can only read is refused for an edit.
func TestDocAIRefusesWhatThePersonCantDoAndRecordsIt(t *testing.T) {
	for _, c := range []struct {
		name string
		d    accessBusiness.Decision
		err  error
		need accessBusiness.Level
		want error
	}{
		{"no such doc", accessBusiness.Decision{}, docBusiness.ErrNotFound, accessBusiness.LevelView, ErrDocAINotFound},
		{"not shared", accessBusiness.Decision{Level: accessBusiness.LevelNone}, nil, accessBusiness.LevelView, ErrDocAINotFound},
		{"in the trash", accessBusiness.Decision{Level: accessBusiness.LevelEdit, Deleted: true}, nil, accessBusiness.LevelView, ErrDocAINotFound},
		{"a viewer asking to edit", accessBusiness.Decision{Level: accessBusiness.LevelView}, nil, accessBusiness.LevelEdit, ErrDocAICantEdit},
		{"an editor of a locked doc", accessBusiness.Decision{Level: accessBusiness.LevelEdit, Locked: true}, nil, accessBusiness.LevelEdit, ErrDocAICantEdit},
	} {
		rows := fakeRecord(t)
		fakeDecision(t, c.d, c.err)
		_, err := CheckDocForAI(context.Background(), member(), recDoc, c.need)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
			continue
		}
		if len(*rows) != 1 || (*rows)[0].action != ActionDocAIReadRefused || (*rows)[0].meta["doc_uuid"] != recDoc || (*rows)[0].kind != "human" {
			t.Errorf("%s: the refusal was not recorded as it should be: %+v", c.name, *rows)
		}
	}
	// Full access edits a locked doc, and a reader reads: nothing to record.
	for _, d := range []accessBusiness.Decision{{Level: accessBusiness.LevelFull, Locked: true}, {Level: accessBusiness.LevelView}} {
		rows := fakeRecord(t)
		fakeDecision(t, d, nil)
		need := accessBusiness.LevelView
		if d.Level == accessBusiness.LevelFull {
			need = accessBusiness.LevelEdit
		}
		if _, err := CheckDocForAI(context.Background(), member(), recDoc, need); err != nil || len(*rows) != 0 {
			t.Errorf("%+v: err=%v rows=%v", d, err, *rows)
		}
	}
	// Not a doc id at all: refused before anything is asked.
	rows := fakeRecord(t)
	fakeDecision(t, accessBusiness.Decision{Level: accessBusiness.LevelFull}, nil)
	if _, err := CheckDocForAI(context.Background(), member(), "Launch notes", accessBusiness.LevelView); !errors.Is(err, ErrDocAINotFound) || len(*rows) != 1 {
		t.Errorf("a name for an id: %v %v", err, *rows)
	}
}

// An accepted AI edit is recorded with what it was and how much changed, and
// never its text.
func TestAnAcceptedEditIsRecordedWithoutItsText(t *testing.T) {
	rows := fakeRecord(t)
	fakeDecision(t, accessBusiness.Decision{Level: accessBusiness.LevelEdit}, nil)
	hash := strings.Repeat("ab", 32)
	err := RecordDocAIEdit(context.Background(), member(), DocAIEdit{
		DocUUID: recDoc, Action: "rewrite", Model: "qwen3 4B", Local: true, Changes: 2, Of: 3, BeforeHash: hash, AfterHash: hash, Chars: 120,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(*rows) != 1 {
		t.Fatalf("rows: %+v", *rows)
	}
	r := (*rows)[0]
	if r.action != ActionDocAIEdit || r.kind != "human" || r.meta["action"] != "improve" || r.meta["changes"] != 2 || r.meta["of"] != 3 || r.meta["local"] != true || r.meta["before_hash"] != hash {
		t.Errorf("row: %+v", r)
	}
	if r.summary != "Accepted an AI rewrite (2 of 3 changes), written by qwen3 4B on your server" {
		t.Errorf("summary: %q", r.summary)
	}
	for k := range r.meta {
		if k == "text" || k == "before" || k == "after" || k == "content" {
			t.Errorf("the row keeps the text under %q", k)
		}
	}
	remote := fakeRecord(t)
	_ = RecordDocAIEdit(context.Background(), member(), DocAIEdit{DocUUID: recDoc, Action: "summarize", Model: "gpt-4.1-mini", Inserted: true})
	if got := (*remote)[0].summary; got != "Inserted an AI summary, written by gpt-4.1-mini outside your server" {
		t.Errorf("remote summary: %q", got)
	}
	// Without the model, the row doesn't guess where it ran.
	unknown := fakeRecord(t)
	_ = RecordDocAIEdit(context.Background(), member(), DocAIEdit{DocUUID: recDoc, Action: "fix", Changes: 1, Of: 1})
	if got := (*unknown)[0].summary; got != "Accepted an AI spelling and grammar fix" {
		t.Errorf("summary without a model: %q", got)
	}

	for _, bad := range []DocAIEdit{
		{DocUUID: recDoc, Action: "delete_everything"},
		{DocUUID: recDoc, Action: "improve", BeforeHash: "not-a-hash"},
		{DocUUID: recDoc, Action: "improve", Changes: 4, Of: 3},
	} {
		var invalid *DocAIInvalidError
		if err := RecordDocAIEdit(context.Background(), member(), bad); !errors.As(err, &invalid) {
			t.Errorf("%+v: got %v, want an invalid request", bad, err)
		}
	}
	// A viewer can't record an edit into a doc they can't change.
	fakeDecision(t, accessBusiness.Decision{Level: accessBusiness.LevelView}, nil)
	if err := RecordDocAIEdit(context.Background(), member(), DocAIEdit{DocUUID: recDoc, Action: "improve"}); !errors.Is(err, ErrDocAICantEdit) {
		t.Errorf("a viewer's edit: %v", err)
	}
	// A reader may ask for a summary (it needs only view: DocActionNeeds),
	// and read it; putting it into the doc is an edit, and is refused (docs
	// incident follow-up, 15 Oct 2026).
	if DocActionNeeds("summarize") != accessBusiness.LevelView {
		t.Error("asking for a summary needs more than view")
	}
	if err := RecordDocAIEdit(context.Background(), member(), DocAIEdit{DocUUID: recDoc, Action: "summarize", Inserted: true, Changes: 1, Of: 1}); !errors.Is(err, ErrDocAICantEdit) {
		t.Errorf("a viewer's inserted summary: %v, want refused", err)
	}
}

// "AI activity on this doc" is for people who can read the doc: anyone else
// learns nothing, and the log is not read for them.
func TestAIActivityOnADocIsForItsReaders(t *testing.T) {
	rows := fakeRecord(t)
	saved := listDocAIEntries
	t.Cleanup(func() { listDocAIEntries = saved })
	read := 0
	meta := `{"doc_uuid":"` + recDoc + `","model":"qwen3 4B","local":true,"agent_name":"Release Captain","run_id":"r1"}`
	listDocAIEntries = func(_ context.Context, doc, prefix string, limit int) ([]*auditModel.AuditEntry, error) {
		read++
		if doc != recDoc || prefix != "ai.doc." || limit != 50 {
			t.Errorf("read %s %s %d", doc, prefix, limit)
		}
		return []*auditModel.AuditEntry{{Seq: 7, Action: ActionDocAgentEditAllowed, Summary: "Release Captain added to a doc", ActorKind: "agent", CreatedAt: time.Unix(100, 0), Metadata: &meta, EntryHash: "e", PrevHash: "p"}}, nil
	}
	fakeDecision(t, accessBusiness.Decision{Level: accessBusiness.LevelNone}, nil)
	if _, err := DocAIActivity(context.Background(), member(), recDoc, 0); !errors.Is(err, ErrDocAINotFound) || read != 0 {
		t.Fatalf("an outsider: err=%v reads=%d", err, read)
	}
	// Only looking writes nothing to the record: polling a doc's id adds no rows.
	if len(*rows) != 0 {
		t.Fatalf("a refused look was recorded: %+v", *rows)
	}
	fakeDecision(t, accessBusiness.Decision{Level: accessBusiness.LevelView}, nil)
	items, err := DocAIActivity(context.Background(), member(), recDoc, 0)
	if err != nil || len(items) != 1 {
		t.Fatalf("a reader: %v %v", items, err)
	}
	it := items[0]
	if it.Status != "allowed" || it.AgentName != "Release Captain" || it.RunID != "r1" || it.Model != "qwen3 4B" || it.Local == nil || !*it.Local || it.Seq != 7 || it.EntryHash != "e" {
		t.Errorf("item: %+v", it)
	}
}
