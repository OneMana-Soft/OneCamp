package docController

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	business "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
)

// openDocPage asks GetDocPage for a doc as a member who may read it.
func openDocPage(t *testing.T, docUUID string) (int, map[string]any) {
	t.Helper()
	var me userModels.UserInfo
	me.UserDgraphInfo.Uid, me.UserDgraphInfo.Uuid = "0x2a", "u-reader"
	route := chi.NewRouteContext()
	route.URLParams.Add("doc_uuid", docUUID)
	ctx := context.WithValue(context.WithValue(context.Background(), helpers.UserInfoContextKey, me), chi.RouteCtxKey, route)
	rec := httptest.NewRecorder()
	GetDocPage(rec, httptest.NewRequest(http.MethodGet, "/doc/getDocInfo/"+docUUID, nil).WithContext(ctx))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not JSON: %s", rec.Body.String())
	}
	return rec.Code, body
}

// A doc whose ending grants can't be read isn't opened (503): its counts would
// be the ones before a grant ended, so a person whose access just ran out could
// still read it. The page says to try again; nothing of the doc is sent (B6
// audit: the 503 had no test).
func TestADocIsNotOpenedWhenItsEndingGrantsCantBeRead(t *testing.T) {
	savedDoc, savedNow := docForPage, accessNow
	t.Cleanup(func() { docForPage, accessNow = savedDoc, savedNow })
	const id = "6f1c0d5e-8a39-4c55-9d5e-2f3f1b2a7c10"
	docForPage = func(context.Context, string, string) (*dgraphStruct.DgraphDoc, error) {
		return &dgraphStruct.DgraphDoc{Uuid: id, Title: "Launch plan", HasReadAccess: 1}, nil
	}
	asked := false
	accessNow = func(context.Context, *dgraphStruct.DgraphDoc, business.Viewer) (business.DocDecision, error) {
		asked = true
		return business.DocDecision{}, errors.New("doc_grants: connection refused")
	}
	code, body := openDocPage(t, id)
	if !asked {
		t.Fatal("the ending grants were never read")
	}
	if code != http.StatusServiceUnavailable || body["msg"] != "Couldn't open the doc just now. Try again in a moment." {
		t.Errorf("got %d %v", code, body)
	}
	if _, sent := body["data"]; sent {
		t.Errorf("the doc was sent though its grants couldn't be read: %v", body["data"])
	}

	// The same doc, once the grants can be read, opens.
	accessNow = func(_ context.Context, doc *dgraphStruct.DgraphDoc, v business.Viewer) (business.DocDecision, error) {
		return business.DecideDoc(doc, v), nil
	}
	if code, body := openDocPage(t, id); code != http.StatusOK || body["data"] == nil {
		t.Errorf("a readable doc: %d %v", code, body)
	}
}
