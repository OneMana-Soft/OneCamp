package clickup

// Pure-logic tests for the ClickUp provider. Network paths (snapshot
// loader, REST calls) belong in the integration suite where we can
// stand up a httptest server.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

func TestCommentTextToHTML_BasicShape(t *testing.T) {
	in := "First paragraph.\n\nSecond paragraph."
	got := commentTextToHTML(in)
	if !strings.Contains(got, "<p>First paragraph.</p>") || !strings.Contains(got, "<p>Second paragraph.</p>") {
		t.Fatalf("expected two paragraphs, got %q", got)
	}
}

func TestCommentTextToHTML_HtmlEscape(t *testing.T) {
	in := `<img onerror="alert(1)">`
	got := commentTextToHTML(in)
	if strings.Contains(got, "<img") {
		t.Fatalf("expected raw <img> to be escaped, got %q", got)
	}
}

func TestParseClickUpMillis_Variants(t *testing.T) {
	cases := []struct {
		in   string
		zero bool
	}{
		{"", true},
		{"0", true},
		{"abc", true},
		{"1700000000000", false},
	}
	for _, tc := range cases {
		got := parseClickUpMillis(tc.in)
		if tc.zero {
			if got != nil {
				t.Fatalf("input %q: expected nil, got %v", tc.in, got)
			}
		} else {
			if got == nil || got.IsZero() {
				t.Fatalf("input %q: expected non-zero time, got %v", tc.in, got)
			}
		}
	}
}

func TestParseClickUpMillis_Roundtrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	in := now.UnixMilli()
	got := parseClickUpMillis(formatInt(in))
	if got == nil {
		t.Fatal("expected parsed time")
	}
	if !got.Equal(now) {
		t.Fatalf("round-trip mismatch: got %v want %v", got, now)
	}
}

func formatInt(n int64) string {
	// avoid pulling strconv into the test file just for one call
	b := []byte{}
	if n == 0 {
		return "0"
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestCapabilities_HasExpectedFlags(t *testing.T) {
	p := New()
	caps := p.Capabilities()
	for _, want := range []importProvider.Capability{
		importProvider.CapTeams,
		importProvider.CapProjects,
		importProvider.CapTasks,
		importProvider.CapSubtasks,
		importProvider.CapTaskComments,
		importProvider.CapAttachments,
	} {
		if caps&want == 0 {
			t.Fatalf("expected capability bit %d to be set", want)
		}
	}
}

func TestDefaultStatusMap_LowerCaseKeys(t *testing.T) {
	p := New()
	for k := range p.DefaultStatusMap() {
		if k != strings.ToLower(k) {
			t.Fatalf("status map key %q is not lower-case", k)
		}
	}
}

func TestDefaultPriorityMap_LowerCaseKeys(t *testing.T) {
	p := New()
	for k := range p.DefaultPriorityMap() {
		if k != strings.ToLower(k) {
			t.Fatalf("priority map key %q is not lower-case", k)
		}
	}
}

// TestClickupTask_UnmarshalJSON confirms the custom UnmarshalJSON
// flattens nested objects. The embedded fields (StatusName,
// PriorityLabel, ListID, ParentID) come from the response's nested
// shapes; without flattening every getter would have to dig through
// pointers.
func TestClickupTask_UnmarshalJSON(t *testing.T) {
	raw := []byte(`{
		"id": "task-1",
		"name": "Fix navbar",
		"status": {"status": "In Progress", "type": "custom"},
		"priority": {"id": "2", "priority": "High"},
		"creator": {"id": 42, "username": "alice"},
		"list":   {"id": "list-7"},
		"folder": {"id": "folder-3"},
		"space":  {"id": "space-9"},
		"parent": "task-parent",
		"assignees": [{"id": 99, "username": "bob"}],
		"date_created": "1700000000000"
	}`)
	var task clickupTask
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if task.StatusName != "In Progress" || task.StatusType != "custom" {
		t.Fatalf("status flatten failed: %+v", task)
	}
	if task.PriorityLabel != "High" || task.PriorityID != "2" {
		t.Fatalf("priority flatten failed: %+v", task)
	}
	if task.CreatorID != "42" {
		t.Fatalf("creator id should be string-normalised: got %q", task.CreatorID)
	}
	if task.ListID != "list-7" || task.FolderID != "folder-3" || task.SpaceID != "space-9" {
		t.Fatalf("ref id flatten failed: %+v", task)
	}
	if task.ParentID != "task-parent" {
		t.Fatalf("parent id flatten failed: %q", task.ParentID)
	}
	if len(task.Assignees) != 1 || task.Assignees[0].ID != "99" {
		t.Fatalf("assignee id flatten failed: %+v", task.Assignees)
	}
}

func TestClickupTask_UnmarshalJSON_NullParent(t *testing.T) {
	raw := []byte(`{
		"id": "task-2",
		"name": "Top-level",
		"status": {"status": "Open", "type": "open"},
		"parent": null
	}`)
	var task clickupTask
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if task.ParentID != "" {
		t.Fatalf("expected empty parent id, got %q", task.ParentID)
	}
}

func TestTaskToSourceTask_DefaultsAndAttachments(t *testing.T) {
	p := New()
	createdMs := int64(1700000000000)
	due := int64(1700100000000)
	in := clickupTask{
		ID:          "task-1",
		Name:        "Fix navbar",
		TextContent: "Body",
		StatusName:  "In Progress",
		StatusType:  "custom",
		// Empty PriorityLabel forces fallback to "normal".
		DateCreated: formatInt(createdMs),
		DueDate:     formatInt(due),
		ListID:      "list-7",
		Attachments: []clickupAttachment{
			{ID: "a1", Title: "shot.png", URL: "https://files.clickup.com/x.png", Size: 1024, MimeType: "image/png"},
			{ID: "a2", Title: "no-url"}, // skipped because URL empty
		},
		Assignees: []clickupUser{{ID: "u1"}},
		Tags:      []clickupTag{{Name: "bug"}},
	}
	out := p.taskToSourceTask(in)

	if out.SourceID != "task-1" {
		t.Fatalf("unexpected source id %q", out.SourceID)
	}
	if out.Status != "In Progress" {
		t.Fatalf("unexpected status %q", out.Status)
	}
	if out.Priority != "normal" {
		t.Fatalf("expected priority fallback 'normal', got %q", out.Priority)
	}
	if len(out.AttachmentRefs) != 1 {
		t.Fatalf("expected exactly one attachment (no-url skipped), got %d", len(out.AttachmentRefs))
	}
	if out.AttachmentRefs[0].Mime != "image/png" {
		t.Fatalf("mime not propagated: %+v", out.AttachmentRefs[0])
	}
	if len(out.Labels) != 1 || out.Labels[0] != "bug" {
		t.Fatalf("tags not mapped: %+v", out.Labels)
	}
	if len(out.AssigneeIds) != 1 || out.AssigneeIds[0] != "u1" {
		t.Fatalf("assignees not mapped: %+v", out.AssigneeIds)
	}
}
