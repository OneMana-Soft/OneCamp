package business

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ai "github.com/akashc777/OneCamp/services/AI"
)

const testDoc = "11111111-2222-3333-4444-555555555555"

func TestAppendToDocChecksEditAccessBeforeSending(t *testing.T) {
	savedCan, savedAppend := canEditDoc, collabAppend
	t.Cleanup(func() { canEditDoc, collabAppend = savedCan, savedAppend })
	sent := 0
	collabAppend = func(_ context.Context, doc, html, editor string) (int, error) {
		sent++
		if !strings.Contains(html, "<ol><li>") {
			t.Fatalf("Markdown should arrive as HTML the editor understands: %q", html)
		}
		return 2, nil
	}
	act := ai.ProposedAction{ToolName: "append_to_doc", Params: map[string]string{"doc_uuid": testDoc, "content": "## Rollback\n1. Revert the deploy\n2. Restore the snapshot"}}

	canEditDoc = func(context.Context, string, string) (bool, error) { return false, nil }
	if _, _, err := executeAppendToDoc(context.Background(), act, "u-1"); err == nil || sent != 0 {
		t.Fatalf("a reader who cannot edit must be refused before anything is sent (err=%v sent=%d)", err, sent)
	}
	canEditDoc = func(context.Context, string, string) (bool, error) { return false, errors.New("dgraph down") }
	if _, _, err := executeAppendToDoc(context.Background(), act, "u-1"); err == nil || sent != 0 {
		t.Fatal("an unknown answer about access must refuse, not send")
	}
	canEditDoc = func(context.Context, string, string) (bool, error) { return true, nil }
	out, _, err := executeAppendToDoc(context.Background(), act, "u-1")
	if err != nil || sent != 1 || !strings.Contains(out, "Added 2 block(s)") {
		t.Fatalf("editor: out=%q err=%v sent=%d", out, err, sent)
	}
	for _, bad := range []map[string]string{{"doc_uuid": "Launch sync notes", "content": "x"}, {"doc_uuid": testDoc, "content": "  "}} {
		if _, _, err := executeAppendToDoc(context.Background(), ai.ProposedAction{Params: bad}, "u-1"); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}

func TestPostCollabAppendSpeaksTheServiceContract(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/docs/"+testDoc+"/append" || r.Header.Get("X-Internal-Secret") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"msg":"not authorised"}`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"data":{"appended_blocks":3}}`))
	}))
	defer srv.Close()
	t.Setenv("COLLAB_INTERNAL_URL", srv.URL)

	t.Setenv("INTERNAL_SECRET", "")
	if _, err := postCollabAppend(context.Background(), testDoc, "<p>x</p>", "bot-1"); err == nil {
		t.Fatal("without a secret there must be no request at all")
	}
	t.Setenv("INTERNAL_SECRET", "wrong")
	if _, err := postCollabAppend(context.Background(), testDoc, "<p>x</p>", "bot-1"); err == nil || !strings.Contains(err.Error(), "not authorised") {
		t.Fatalf("a refusal must be reported with its reason: %v", err)
	}
	t.Setenv("INTERNAL_SECRET", "k")
	n, err := postCollabAppend(context.Background(), testDoc, "<p>x</p>", "bot-1")
	if err != nil || n != 3 || got["html"] != "<p>x</p>" || got["editor_uuid"] != "bot-1" {
		t.Fatalf("n=%d err=%v body=%v", n, err, got)
	}
}
