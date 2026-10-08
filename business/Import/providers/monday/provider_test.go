package monday

// Pure tests: column parsing exactly as monday returns column_values
// (`text` plus a JSON-string `value`), the item → task mapping, and the
// provider's static surface. Network behaviour lives in transport_test.go.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// cv builds a column_value the way the API sends it: `value` is a JSON
// string holding JSON (or null).
func cv(id, typ, title, text, value string) mondayColumnValue {
	c := mondayColumnValue{ID: id, Type: typ}
	if text != "" {
		t := text
		c.Text = &t
	}
	if value == "" {
		c.Value = json.RawMessage("null")
	} else {
		b, _ := json.Marshal(value)
		c.Value = b
	}
	c.Column = &struct {
		Title string `json:"title"`
	}{Title: title}
	return c
}

func day(s string) time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return t
}

func TestParseStatus_UsesText(t *testing.T) {
	c := cv("status", "status", "Status", "Working on it", `{"index":0,"post_id":null,"changed_at":"2026-09-01T10:00:00.000Z"}`)
	if got := parseLabel(c); got != "Working on it" {
		t.Fatalf("status label = %q", got)
	}
}

func TestParseLabel_StripsDefaultEmoji(t *testing.T) {
	c := cv("priority", "status", "Priority", "Critical ⚠️️", `{"index":4}`)
	if got := parseLabel(c); got != "Critical" {
		t.Fatalf("priority label = %q, want Critical", got)
	}
}

func TestParseDate_ValueWithTimeIsUTC(t *testing.T) {
	c := cv("date4", "date", "Due date", "2026-10-08 19:30", `{"date":"2026-10-08","time":"14:00:00","changed_at":"2026-09-01T10:00:00.000Z"}`)
	got := parseDateValue(c)
	want := time.Date(2026, 10, 8, 14, 0, 0, 0, time.UTC)
	if got == nil || !got.Equal(want) {
		t.Fatalf("date = %v, want %v", got, want)
	}
}

func TestParseDate_DateOnlyAndTextFallback(t *testing.T) {
	if got := parseDateValue(cv("d", "date", "Date", "2026-10-08", `{"date":"2026-10-08"}`)); got == nil || !got.Equal(day("2026-10-08")) {
		t.Fatalf("date-only value: %v", got)
	}
	// value null (some versions) → the text still carries the day.
	if got := parseDateValue(cv("d", "date", "Date", "2026-11-02 09:15", "")); got == nil || !got.Equal(day("2026-11-02")) {
		t.Fatalf("text fallback: %v", got)
	}
	if got := parseDateValue(cv("d", "date", "Date", "", "")); got != nil {
		t.Fatalf("empty date should be nil, got %v", got)
	}
}

func TestParseTimeline_ValueAndText(t *testing.T) {
	from, to := parseTimelineValue(cv("timeline", "timeline", "Timeline", "2026-10-01 - 2026-10-10",
		`{"from":"2026-10-01","to":"2026-10-10","changed_at":"2026-09-01T10:00:00.000Z"}`))
	if from == nil || to == nil || !from.Equal(day("2026-10-01")) || !to.Equal(day("2026-10-10")) {
		t.Fatalf("timeline value: %v → %v", from, to)
	}
	from, to = parseTimelineValue(cv("timeline", "timerange", "Timeline", "2026-12-01 - 2026-12-24", ""))
	if from == nil || to == nil || !from.Equal(day("2026-12-01")) || !to.Equal(day("2026-12-24")) {
		t.Fatalf("timeline text fallback: %v → %v", from, to)
	}
}

func TestParsePeople_SkipsTeamsAndAcceptsNumericIDs(t *testing.T) {
	c := cv("person", "people", "Owner", "Ada, Grace, Design",
		`{"changed_at":"2026-09-01T10:00:00.000Z","personsAndTeams":[{"id":4012,"kind":"person"},{"id":"4013","kind":"person"},{"id":77,"kind":"team"},{"id":4012,"kind":"person"}]}`)
	got := parsePeopleValue(c)
	if strings.Join(got, ",") != "4012,4013" {
		t.Fatalf("people = %v, want [4012 4013]", got)
	}
	if got := parsePeopleValue(cv("person", "people", "Owner", "", "")); len(got) != 0 {
		t.Fatalf("empty people column should yield nothing, got %v", got)
	}
}

func TestParseTags_FromText(t *testing.T) {
	c := cv("tags", "tags", "Tags", "bug, ui , ", `{"tag_ids":[12,13]}`)
	got := parseTagsValue(c)
	if strings.Join(got, "|") != "bug|ui" {
		t.Fatalf("tags = %v", got)
	}
}

func TestReadItemFields_FullBoard(t *testing.T) {
	item := mondayItem{
		ID:    "101",
		Name:  "Ship the importer",
		Group: &mondayGroup{ID: "topics", Title: "This week"},
		ColumnValues: []mondayColumnValue{
			cv("person", "people", "Owner", "Ada", `{"personsAndTeams":[{"id":4012,"kind":"person"}]}`),
			cv("status", "status", "Status", "Stuck", `{"index":2}`),
			cv("priority", "status", "Priority", "High", `{"index":1}`),
			cv("timeline", "timeline", "Timeline", "2026-10-01 - 2026-10-10", `{"from":"2026-10-01","to":"2026-10-10"}`),
			cv("date4", "date", "Due date", "2026-10-12", `{"date":"2026-10-12"}`),
			cv("tags", "tags", "Tags", "backend, import", `{"tag_ids":[1,2]}`),
			cv("text", "text", "Customer", "Acme", `"Acme"`),
			cv("numbers", "numbers", "Estimate", "", ""),
		},
	}
	f := readItemFields(item, nil)
	if f.Status != "Stuck" {
		t.Fatalf("status = %q", f.Status)
	}
	if f.Priority != "High" {
		t.Fatalf("priority = %q", f.Priority)
	}
	if len(f.Assignees) != 1 || f.Assignees[0] != "4012" {
		t.Fatalf("assignees = %v", f.Assignees)
	}
	if f.Start == nil || !f.Start.Equal(day("2026-10-01")) {
		t.Fatalf("start = %v (timeline from)", f.Start)
	}
	// An explicit due-date column beats the timeline's end.
	if f.Due == nil || !f.Due.Equal(day("2026-10-12")) {
		t.Fatalf("due = %v", f.Due)
	}
	if strings.Join(f.Labels, ",") != "backend,import" {
		t.Fatalf("labels = %v", f.Labels)
	}
	if len(f.Extra) != 1 || f.Extra[0][0] != "Customer" || f.Extra[0][1] != "Acme" {
		t.Fatalf("unmapped columns = %v", f.Extra)
	}
}

func TestReadItemFields_TimelineGivesBothDates(t *testing.T) {
	f := readItemFields(mondayItem{ColumnValues: []mondayColumnValue{
		cv("timeline", "timeline", "Timeline", "", `{"from":"2026-10-01","to":"2026-10-10"}`),
	}}, nil)
	if f.Start == nil || f.Due == nil || !f.Start.Equal(day("2026-10-01")) || !f.Due.Equal(day("2026-10-10")) {
		t.Fatalf("timeline → %v / %v", f.Start, f.Due)
	}
}

func TestReadItemFields_GroupTitleWhenNoStatus(t *testing.T) {
	f := readItemFields(mondayItem{Group: &mondayGroup{Title: "Done"}, ColumnValues: []mondayColumnValue{
		cv("date", "date", "Date", "2026-10-03", `{"date":"2026-10-03"}`),
	}}, nil)
	if f.Status != "Done" {
		t.Fatalf("status from group = %q", f.Status)
	}
	if f.Due == nil || !f.Due.Equal(day("2026-10-03")) {
		t.Fatalf("a lone untitled date column is the due date, got %v", f.Due)
	}
}

func TestReadItemFields_PrefersColumnTitledStatus(t *testing.T) {
	f := readItemFields(mondayItem{ColumnValues: []mondayColumnValue{
		cv("color1", "color", "QA", "Passed", `{"index":1}`),
		cv("status", "status", "Status", "Done", `{"index":1}`),
	}}, nil)
	if f.Status != "Done" {
		t.Fatalf("status = %q, want the column titled Status", f.Status)
	}
}

func TestItemToSourceTask_Mapping(t *testing.T) {
	p := New()
	created := "2026-09-01T10:00:00Z"
	creator := "4012"
	item := mondayItem{
		ID: "101", Name: "Ship it", URL: "https://acme.monday.com/boards/1/pulses/101",
		CreatedAt: &created, CreatorID: &creator, State: "active",
		Group: &mondayGroup{Title: "This week"},
		ColumnValues: []mondayColumnValue{
			cv("status", "status", "Status", "Done", `{"index":1}`),
		},
		Assets: []mondayAsset{
			{ID: "a1", Name: "spec.pdf", PublicURL: "https://files.monday.com/a1", FileSize: 1234},
			{ID: "a2", Name: "from-update.png"},
		},
		Subitems: []mondayItem{{ID: "201"}},
	}
	st := p.itemToSourceTask(item, "1", "", 3, map[string]bool{"a2": true}, fieldColumns{})
	if st.SourceID != "101" || st.ProjectSourceID != "1" || st.ParentTaskID != "" {
		t.Fatalf("ids: %+v", st)
	}
	if !st.Completed {
		t.Fatal("Done should mark the task completed")
	}
	if st.CreatedBy != "4012" || !st.Created.Equal(time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("creator/created: %q %v", st.CreatedBy, st.Created)
	}
	if len(st.AttachmentRefs) != 1 || st.AttachmentRefs[0].SourceID != "a1" || st.AttachmentRefs[0].Mime != "application/pdf" {
		t.Fatalf("attachments (update assets must be excluded): %+v", st.AttachmentRefs)
	}
	if st.CommentCount != 3 || st.SubtaskCount != 1 {
		t.Fatalf("hints: comments=%d subtasks=%d", st.CommentCount, st.SubtaskCount)
	}
	if st.Metadata["monday_url"] != item.URL || st.Metadata["monday_group"] != "This week" {
		t.Fatalf("metadata: %v", st.Metadata)
	}
}

func TestRenderExtra_EscapesHTML(t *testing.T) {
	got := renderExtra([][2]string{{"Notes", `<script>x</script>`}})
	if strings.Contains(got, "<script>") || !strings.Contains(got, "&lt;script&gt;") {
		t.Fatalf("unescaped: %q", got)
	}
}

func TestUpdatesToComments_RepliesSanitisedAndOrdered(t *testing.T) {
	t1, t2, t3 := "2026-09-02T10:00:00Z", "2026-09-01T10:00:00Z", "2026-09-03T10:00:00Z"
	u1, u2 := "4012", "4013"
	cs := updatesToComments("101", []mondayUpdate{
		{ID: "u1", Body: `<p>hello <script>alert(1)</script><b>team</b></p>`, CreatedAt: &t1, CreatorID: &u1,
			Assets:  []mondayAsset{{ID: "a2", Name: "shot.png"}},
			Replies: []mondayReply{{ID: "r1", Body: "<p>thanks</p>", CreatedAt: &t3, CreatorID: &u2}}},
		{ID: "u0", TextBody: "plain only", CreatedAt: &t2},
	})
	if len(cs) != 3 {
		t.Fatalf("want 3 comments (2 updates + 1 reply), got %d", len(cs))
	}
	if cs[0].SourceID != "u0" || cs[1].SourceID != "u1" || cs[2].SourceID != "r1" {
		t.Fatalf("not chronological: %s %s %s", cs[0].SourceID, cs[1].SourceID, cs[2].SourceID)
	}
	if strings.Contains(cs[1].Body, "<script>") || !strings.Contains(cs[1].Body, "<b>team</b>") {
		t.Fatalf("sanitiser: %q", cs[1].Body)
	}
	if len(cs[1].AttachmentRefs) != 1 || cs[1].AttachmentRefs[0].Parent.Kind != "comment" || cs[1].AttachmentRefs[0].Parent.SourceID != "u1" {
		t.Fatalf("update asset should attach to the comment: %+v", cs[1].AttachmentRefs)
	}
	if !strings.Contains(cs[0].Body, "plain only") {
		t.Fatalf("text_body fallback: %q", cs[0].Body)
	}
	if cs[2].AuthorSourceID != "4013" || cs[2].TaskSourceID != "101" {
		t.Fatalf("reply author/task: %+v", cs[2])
	}
}

func TestDefaultMaps_LowerCaseAndValid(t *testing.T) {
	p := New()
	for k, v := range p.DefaultStatusMap() {
		if k != strings.ToLower(k) {
			t.Fatalf("status key %q not lower-case", k)
		}
		if importProvider.ApplyStatusMap(k, nil, p.DefaultStatusMap()) != v {
			t.Fatalf("status %q → %q is not a valid OneCamp status", k, v)
		}
	}
	for k, v := range p.DefaultPriorityMap() {
		if k != strings.ToLower(k) {
			t.Fatalf("priority key %q not lower-case", k)
		}
		if importProvider.ApplyPriorityMap(k, nil, p.DefaultPriorityMap()) != v {
			t.Fatalf("priority %q → %q is not valid", k, v)
		}
	}
	for label, want := range map[string]string{
		"Working on it": "inProgress", "Done": "done", "Not Started": "todo", "Waiting for review": "inReview",
	} {
		if got := importProvider.ApplyStatusMap(label, nil, p.DefaultStatusMap()); got != want {
			t.Fatalf("%q → %q, want %q", label, got, want)
		}
	}
}

func TestCapabilitiesAndRegistration(t *testing.T) {
	p := New()
	for _, want := range []importProvider.Capability{
		importProvider.CapTeams, importProvider.CapProjects, importProvider.CapTasks,
		importProvider.CapSubtasks, importProvider.CapTaskComments, importProvider.CapAttachments,
	} {
		if p.Capabilities()&want == 0 {
			t.Fatalf("missing capability %d", want)
		}
	}
	if importProvider.Get("monday") == nil {
		t.Fatal("monday provider not registered by init()")
	}
	var _ importProvider.Discoverer = p
	var _ importProvider.JobCleaner = p
}

func TestScopeFrom_OptionsAndJobBlob(t *testing.T) {
	j := &importModels.Job{Options: []byte(`{"workspace_id":"55","board_ids":["1","2"],"include_archived":true}`)}
	sc := scopeFrom(j, nil)
	if sc.WorkspaceID != "55" || !sc.BoardIDs["1"] || !sc.BoardIDs["2"] || !sc.IncludeArchived {
		t.Fatalf("scope from job blob: %+v", sc)
	}
	sc = scopeFrom(&importModels.Job{}, importProvider.JobOptions{"board_ids": "7, 8"})
	if !sc.BoardIDs["7"] || !sc.BoardIDs["8"] || sc.WorkspaceID != "" {
		t.Fatalf("scope from comma list: %+v", sc)
	}
}

func TestBoardImportable(t *testing.T) {
	for typ, want := range map[string]bool{"board": true, "": true, "document": false, "sub_items_board": false, "custom_object": false} {
		if got := (mondayBoard{Type: typ}).importable(); got != want {
			t.Fatalf("type %q importable=%v, want %v", typ, got, want)
		}
	}
	ws := "-1"
	if (mondayBoard{WorkspaceID: &ws}).workspaceKey() != mainWorkspaceID || (mondayBoard{}).workspaceKey() != mainWorkspaceID {
		t.Fatal("null / -1 workspace should map to the Main workspace team")
	}
}
