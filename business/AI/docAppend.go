package business

// append_to_doc: an agent adds to an existing document.
//
// The collaboration service applies the addition as a Yjs update through a
// server-side connection (other-services/collaboration-service/appendToDoc.js),
// so it merges like any other edit and people with the doc open see it arrive.
// That is what makes this safe where writing doc_body was not (see docAgent.go).
//
// Permission is the app's own edit rule (CheckUserDocEditAccess: an editor or
// the owner), checked here as the acting principal before anything is sent.
// The collaboration service trusts only the shared INTERNAL_SECRET.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

const (
	appendMaxContentRunes = 20000
	collabAppendTimeout   = 15 * time.Second
	defaultCollabURL      = "http://collaboration-service:1234"
)

// Seams, so the rules are tested without the graph or the collaboration service.
var (
	canEditDoc = func(ctx context.Context, docUUID, userUUID string) (bool, error) {
		dgraphUser, err := getDgraphUserForExecutor(ctx, userUUID)
		if err != nil {
			return false, err
		}
		return docBusiness.CheckUserDocEditAccess(ctx, docUUID, dgraphUser.Uid)
	}
	collabAppend = postCollabAppend
)

func executeAppendToDoc(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	docUUID := strings.TrimSpace(action.Params["doc_uuid"])
	if _, err := uuid.Parse(docUUID); err != nil {
		return "", nil, fmt.Errorf("doc_uuid must be a document's UUID; find it with search_workspace")
	}
	content := strings.TrimSpace(action.Params["content"])
	if content == "" {
		return "", nil, fmt.Errorf("content is required")
	}
	if len([]rune(content)) > appendMaxContentRunes {
		return "", nil, fmt.Errorf("content is too long to add at once (%d characters max); add it in parts", appendMaxContentRunes)
	}
	ok, err := canEditDoc(ctx, docUUID, userUUID)
	if err != nil || !ok {
		// One answer for "no such doc" and "not yours to edit", so the tool
		// cannot be used to learn which private documents exist.
		return "", nil, fmt.Errorf("document not found, or you are not allowed to edit it")
	}
	blocks, err := collabAppend(ctx, docUUID, docMarkdownToHTML(content), userUUID)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("Added %d block(s) to the end of the document. Everything that was already there is unchanged.", blocks),
		map[string]string{"doc_uuid": docUUID}, nil
}

// postCollabAppend asks the collaboration service to append html to a doc.
func postCollabAppend(ctx context.Context, docUUID, html, editorUUID string) (int, error) {
	secret := strings.TrimSpace(os.Getenv("INTERNAL_SECRET"))
	if secret == "" {
		return 0, fmt.Errorf("editing documents is not available: INTERNAL_SECRET is not set on this server")
	}
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("COLLAB_INTERNAL_URL")), "/")
	if base == "" {
		base = defaultCollabURL
	}
	body, _ := json.Marshal(map[string]string{"html": html, "editor_uuid": editorUUID})
	cctx, cancel := context.WithTimeout(ctx, collabAppendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, base+"/internal/docs/"+docUUID+"/append", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Secret", secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("the document service could not be reached; try again shortly")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var out struct {
		Msg  string `json:"msg"`
		Data struct {
			AppendedBlocks int `json:"appended_blocks"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode != http.StatusOK {
		if out.Msg == "" {
			out.Msg = resp.Status
		}
		return 0, fmt.Errorf("the document service refused the edit: %s", out.Msg)
	}
	return out.Data.AppendedBlocks, nil
}
