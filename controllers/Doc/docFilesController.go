package docController

// The doc tree's files over HTTP (docs F1, Docs & Files; router/docsFiles.go):
// a file uploaded into a page or a folder, or at the top of a space; a new
// version of one; and its versions. Who may add to or change a node is the
// Access Resolver's to say, as for any page. An upload is read under the
// workspace's limit (httpx.ReadUpload), then checked, scanned and stored
// (business/Doc AddFile, AddFileVersion). A version downloads through the
// doc's own file route, GET /doc/getFile/{file}/{attachment_id}.

import (
	"context"
	"errors"
	"net/http"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/Doc"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	treeBusiness "github.com/akashc777/OneCamp/business/Tree"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/go-chi/chi/v5"
)

// Seams, so the routes' rules are tested without the stores.
var (
	addFile        = business.AddFile
	addFileVersion = business.AddFileVersion
	fileVersions   = business.FileVersions
	fileRowsOf     = business.DocRows
	indexFiles     = business.IndexFiles
)

// fileFailed answers a file route's error: an upload that wasn't stored in
// the person's words (what was refused, or storage that didn't answer), a
// version's note too long, a node that isn't a file, and everything else as
// the tree answers it.
func fileFailed(w http.ResponseWriter, r *http.Request, where string, err error) {
	switch {
	case errors.Is(err, business.ErrNotStored):
		helpers.WriteUploadFailure(r.Context(), w, err, "The file couldn't be saved. Try again, and tell your admin if it keeps happening.")
	case errors.Is(err, business.ErrNoteTooLong):
		badRequest(w, "A version's note is at most 500 characters.")
	case errors.Is(err, business.ErrNotAFile):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"code": "not_a_file", "msg": "That isn't a file."})
	default:
		treeFailed(w, r, where, err)
	}
}

// fileRow reads the node a file route names, for the person, once the
// Access Resolver has let them at it.
func fileRow(ctx context.Context, docUUID, viewerUID string) (*dgraphStruct.DgraphDoc, error) {
	rows, err := fileRowsOf(ctx, []string{docUUID}, viewerUID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || rows[0] == nil {
		return nil, business.ErrNotFound
	}
	return rows[0], nil
}

// AddDocFile is POST /doc/files (multipart: file, and parent_uuid or space,
// and after_uuid): one file made a node of the tree, its upload its version 1,
// all or nothing, under a page or a folder the person may add to, or at the
// top of a space they're in. It takes its parent's sharing, as a new subpage
// does, and answers {data: TreeNode}: the new node with its file facts.
func AddDocFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, signedIn := httpx.MustUser(w, r)
	if !signedIn {
		return
	}
	who := accessBusiness.SubjectOf(&user)
	file, header, ok := httpx.ReadUpload(w, r, settingsBusiness.UploadLimitMB())
	if !ok {
		return
	}
	defer file.Close()
	in := business.NewFile{ParentUUID: r.FormValue("parent_uuid"), Space: r.FormValue("space"), AfterUUID: r.FormValue("after_uuid"),
		Name: header.Filename, Body: file, Size: header.Size}
	for _, id := range []string{in.ParentUUID, in.AfterUUID} {
		if id != "" && !helpers.IsCanonicalUUID(id) {
			badRequest(w, "That isn't a doc.")
			return
		}
	}
	if _, kind, id := business.TreeSection(in.Space); in.ParentUUID == "" && kind != "" {
		if _, err := accessBusiness.EnterSpace(ctx, who, kind, id); err != nil {
			fileFailed(w, r, "AddDocFile", err)
			return
		}
	}
	var parent *dgraphStruct.DgraphDoc
	if in.ParentUUID != "" {
		if _, err := accessBusiness.Open(ctx, who, docRes(in.ParentUUID), accessBusiness.LevelEdit); err != nil {
			fileFailed(w, r, "AddDocFile", err)
			return
		}
		row, err := fileRow(ctx, in.ParentUUID, who.UserUID)
		if err != nil {
			fileFailed(w, r, "AddDocFile", err)
			return
		}
		parent = row
	}
	doc, facts, err := addFile(ctx, &user.UserDgraphInfo, in, parent)
	if err != nil {
		fileFailed(w, r, "AddDocFile", err)
		return
	}
	if parent != nil {
		if err := accessBusiness.Rules.CopyOnCreate(ctx, who, docRes(parent.Uuid), docRes(doc.Uuid)); err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/AddDocFile: %s didn't take its parent's sharing: %v", doc.Uuid, err)
		}
	}
	// Its search entry, once it has its sharing.
	indexFiles(ctx, []string{doc.Uuid}, "")
	node := treeBusiness.Node{Kind: accessBusiness.KindDoc, UUID: doc.Uuid, ParentUUID: in.ParentUUID, Position: doc.Position,
		Title: doc.Title, CanEdit: true, Data: doc}
	row := treeNodes([]treeBusiness.Node{node}, map[string]*business.FileFacts{doc.Uuid: facts})[0]
	// Its last change is its upload, by the person asking (the new node holds
	// their graph uid only): named as a row reads them.
	row.UpdatedBy = business.PersonOf(&user.UserDgraphInfo)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": row})
}

// fileParam is the file a route names, or a 400.
func fileParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "doc_uuid")
	if !helpers.IsCanonicalUUID(id) {
		badRequest(w, "That isn't a file.")
		return "", false
	}
	return id, true
}

// AddDocFileVersion is POST /doc/files/{doc_uuid}/versions (multipart: file,
// note): "Replace with a new version", for someone who may edit the file. It
// answers {data: version}.
func AddDocFileVersion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, signedIn := httpx.MustUser(w, r)
	if !signedIn {
		return
	}
	who := accessBusiness.SubjectOf(&user)
	id, ok := fileParam(w, r)
	if !ok {
		return
	}
	file, header, ok := httpx.ReadUpload(w, r, settingsBusiness.UploadLimitMB())
	if !ok {
		return
	}
	defer file.Close()
	if _, err := accessBusiness.Open(ctx, who, docRes(id), accessBusiness.LevelEdit); err != nil {
		fileFailed(w, r, "AddDocFileVersion", err)
		return
	}
	row, err := fileRow(ctx, id, who.UserUID)
	if err != nil {
		fileFailed(w, r, "AddDocFileVersion", err)
		return
	}
	v, err := addFileVersion(ctx, &user.UserDgraphInfo, row, business.NewFile{Name: header.Filename, Body: file, Size: header.Size}, r.FormValue("note"))
	if err != nil {
		fileFailed(w, r, "AddDocFileVersion", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// GetDocFileVersions is GET /doc/files/{doc_uuid}/versions: every version of
// a file, newest first, for whoever may open it: {data: {versions: [...]}}.
func GetDocFileVersions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, signedIn := httpx.MustUser(w, r)
	if !signedIn {
		return
	}
	who := accessBusiness.SubjectOf(&user)
	id, ok := fileParam(w, r)
	if !ok {
		return
	}
	if _, err := accessBusiness.Open(ctx, who, docRes(id), accessBusiness.LevelView); err != nil {
		fileFailed(w, r, "GetDocFileVersions", err)
		return
	}
	row, err := fileRow(ctx, id, who.UserUID)
	if err == nil && !business.IsFile(row) {
		err = business.ErrNotAFile
	}
	if err != nil {
		fileFailed(w, r, "GetDocFileVersions", err)
		return
	}
	versions, err := fileVersions(ctx, id)
	if err != nil {
		fileFailed(w, r, "GetDocFileVersions", err)
		return
	}
	if versions == nil {
		versions = []business.FileVersion{}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{"versions": versions}})
}
