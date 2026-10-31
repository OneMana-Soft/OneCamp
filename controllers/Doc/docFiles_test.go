package docController

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/avscan"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

const fileDoc = "5f0c4a2e-8a63-4f3e-9d2b-1c7a9e0b3d41"

// fileRequest is a multipart request as the web sends a file: the file (none
// when name is ""), and the form's other fields, by the person u.
func fileRequest(t *testing.T, method, path, name string, fields map[string]string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if name != "" {
		part, _ := mw.CreateFormFile("file", name)
		_, _ = part.Write([]byte("%PDF-1.4 a contract"))
	}
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	_ = mw.Close()
	r := httptest.NewRequest(method, path, &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	who := userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "b0b00000-0000-4000-8000-000000000002"}}
	return r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, who))
}

// withFileSeams stands in for the stores under the file routes.
func withFileSeams(t *testing.T, rows []*dgraphStruct.DgraphDoc) (indexed *[]string) {
	t.Helper()
	pa, pv, pl, pr, pi, prules := addFile, addFileVersion, fileVersions, fileRowsOf, indexFiles, accessBusiness.Rules
	t.Cleanup(func() {
		addFile, addFileVersion, fileVersions, fileRowsOf, indexFiles, accessBusiness.Rules = pa, pv, pl, pr, pi, prules
	})
	accessBusiness.Rules = fixedRules{d: accessBusiness.Decision{Level: accessBusiness.LevelEdit}}
	fileRowsOf = func(context.Context, []string, string) ([]*dgraphStruct.DgraphDoc, error) { return rows, nil }
	var seen []string
	indexFiles = func(_ context.Context, ids []string, _ string) { seen = append(seen, ids...) }
	return &seen
}

func withChiParam(r *http.Request, key, value string) *http.Request {
	rc := chi.NewRouteContext()
	rc.URLParams.Add(key, value)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc))
}

// A file is added with its upload and answered as a tree row with its facts
// (the F1a/F1b contract, 3), and its search entry is written after it is
// made; a request with no file, or a parent that isn't a doc's id, is a 400;
// an upload the scan blocks is said so, in words.
func TestAFileIsAddedAndAnsweredAsARow(t *testing.T) {
	indexed := withFileSeams(t, nil)
	addFile = func(_ context.Context, _ *dgraphStruct.DgraphUser, in business.NewFile, parent *dgraphStruct.DgraphDoc) (*dgraphStruct.DgraphDoc, *business.FileFacts, error) {
		if in.Name != "msa.pdf" || in.Space != "workspace" || parent != nil {
			t.Errorf("added %+v under %+v", in, parent)
		}
		return &dgraphStruct.DgraphDoc{Uuid: fileDoc, Title: "msa.pdf", NodeKind: "file", Position: "a0"},
			&business.FileFacts{Name: "msa.pdf", SizeBytes: 19, MIME: "application/pdf", Versions: 1, AttachmentID: "att-1"}, nil
	}
	w := httptest.NewRecorder()
	AddDocFile(w, fileRequest(t, http.MethodPost, "/doc/files", "msa.pdf", map[string]string{"space": "workspace"}))
	var out struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	file, _ := out.Data["file"].(map[string]any)
	if w.Code != http.StatusOK || out.Data["uuid"] != fileDoc || out.Data["node_kind"] != "file" || file["attachment_id"] != "att-1" || file["versions"] != float64(1) {
		t.Fatalf("a file: %d %s", w.Code, w.Body)
	}
	// Its last change is its upload, by the person asking (docs F1: "Sam · just now").
	if by, _ := out.Data["updated_by"].(map[string]any); by["user_uuid"] != "b0b00000-0000-4000-8000-000000000002" {
		t.Errorf("a new file's row names %v as its last editor", out.Data["updated_by"])
	}
	if len(*indexed) != 1 || (*indexed)[0] != fileDoc {
		t.Errorf("search entries written for %v", *indexed)
	}

	for name, r := range map[string]*http.Request{
		"no file":       fileRequest(t, http.MethodPost, "/doc/files", "", map[string]string{"space": "workspace"}),
		"a bad parent":  fileRequest(t, http.MethodPost, "/doc/files", "msa.pdf", map[string]string{"parent_uuid": "not-a-doc"}),
		"a bad sibling": fileRequest(t, http.MethodPost, "/doc/files", "msa.pdf", map[string]string{"after_uuid": "x"}),
	} {
		w := httptest.NewRecorder()
		AddDocFile(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}

	addFile = func(context.Context, *dgraphStruct.DgraphUser, business.NewFile, *dgraphStruct.DgraphDoc) (*dgraphStruct.DgraphDoc, *business.FileFacts, error) {
		return nil, nil, errors.Join(business.ErrNotStored, avscan.ErrInfected)
	}
	w = httptest.NewRecorder()
	AddDocFile(w, fileRequest(t, http.MethodPost, "/doc/files", "msa.pdf", nil))
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("a blocked upload: %d %s", w.Code, w.Body)
	}
}

// A new version takes the person's edit on the file (the Access Resolver's
// answer), only of a file (409 for anything else), with its note; and its
// answer is the version (the contract, 4).
func TestANewVersionIsAddedToAFileOnly(t *testing.T) {
	withFileSeams(t, []*dgraphStruct.DgraphDoc{{Uuid: fileDoc, Title: "msa.pdf", NodeKind: "file"}})
	addFileVersion = func(_ context.Context, _ *dgraphStruct.DgraphUser, file *dgraphStruct.DgraphDoc, up business.NewFile, note string) (*business.FileVersion, error) {
		if file.Uuid != fileDoc || up.Name != "msa-signed.pdf" || note != "Signed" {
			t.Errorf("a version of %+v from %+v with %q", file, up, note)
		}
		return &business.FileVersion{N: 2, AttachmentID: "att-2", Name: "msa-signed.pdf", Note: "Signed", CreatedBy: &business.Person{UUID: "u"}, CreatedAt: time.Now()}, nil
	}
	r := withChiParam(fileRequest(t, http.MethodPost, "/doc/files/"+fileDoc+"/versions", "msa-signed.pdf", map[string]string{"note": "Signed"}), "doc_uuid", fileDoc)
	w := httptest.NewRecorder()
	AddDocFileVersion(w, r)
	var out struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != http.StatusOK || out.Data["n"] != float64(2) || out.Data["note"] != "Signed" {
		t.Fatalf("a version: %d %s", w.Code, w.Body)
	}

	withFileSeams(t, []*dgraphStruct.DgraphDoc{{Uuid: fileDoc, Title: "Plan"}})
	addFileVersion = func(context.Context, *dgraphStruct.DgraphUser, *dgraphStruct.DgraphDoc, business.NewFile, string) (*business.FileVersion, error) {
		return nil, business.ErrNotAFile
	}
	w = httptest.NewRecorder()
	AddDocFileVersion(w, withChiParam(fileRequest(t, http.MethodPost, "/doc/files/"+fileDoc+"/versions", "x.pdf", nil), "doc_uuid", fileDoc))
	if w.Code != http.StatusConflict {
		t.Errorf("a version of a page: %d %s", w.Code, w.Body)
	}
}

// A file's versions are listed newest first for whoever may open it (the
// contract, 5: {data: {versions: [...]}}); a page has none to list (409).
func TestAFilesVersionsAreListed(t *testing.T) {
	withFileSeams(t, []*dgraphStruct.DgraphDoc{{Uuid: fileDoc, NodeKind: "file"}})
	fileVersions = func(context.Context, string) ([]business.FileVersion, error) {
		return []business.FileVersion{{N: 2, Name: "b.pdf"}, {N: 1, Name: "a.pdf"}}, nil
	}
	w := httptest.NewRecorder()
	GetDocFileVersions(w, withChiParam(fileRequest(t, http.MethodGet, "/doc/files/"+fileDoc+"/versions", "", nil), "doc_uuid", fileDoc))
	var out struct {
		Data struct {
			Versions []map[string]any `json:"versions"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != http.StatusOK || len(out.Data.Versions) != 2 || out.Data.Versions[0]["n"] != float64(2) {
		t.Fatalf("the versions: %d %s", w.Code, w.Body)
	}
	withFileSeams(t, []*dgraphStruct.DgraphDoc{{Uuid: fileDoc}})
	w = httptest.NewRecorder()
	GetDocFileVersions(w, withChiParam(fileRequest(t, http.MethodGet, "/doc/files/"+fileDoc+"/versions", "", nil), "doc_uuid", fileDoc))
	if w.Code != http.StatusConflict {
		t.Errorf("a page's versions: %d %s", w.Code, w.Body)
	}
}
