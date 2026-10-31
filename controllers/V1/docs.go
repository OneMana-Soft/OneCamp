package controllers

// The public API's docs (/v1/docs): list the docs the token's owner can open,
// read one as Markdown, make one from Markdown, and add Markdown to the end of
// one as a live edit. Reads need docs:read and writes docs:write (on the
// routes, router/docsV1.go). Every call runs as the token's owner through the
// Access Resolver, and a token bound to an agent credits the agent beside its
// owner (I11). Writes are recorded in the audit log, as the API's other
// writes are; a read as Markdown is recorded as an export made through the API.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	appBusiness "github.com/akashc777/OneCamp/business/App"
	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	authService "github.com/akashc777/OneCamp/services/Auth"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/go-chi/chi/v5"
)

// apiDocPage and apiDocPageMax are how many docs a list answers with.
const (
	apiDocPage    = 50
	apiDocPageMax = 100
)

// apiDoc is a doc as the API answers it: as listed, with its address.
type apiDoc struct {
	contentIOBusiness.ListedDoc
	URL      string `json:"url"`
	Markdown string `json:"markdown,omitempty"`
}

// apiSubject is the token's owner as the Access Resolver decides for them,
// with the agent the token is bound to and none of their admin powers
// (accessBusiness.TokenSubjectOf).
func apiSubject(r *http.Request) accessBusiness.Subject {
	u := apiUser(r)
	return accessBusiness.TokenSubjectOf(&u, helpers.GetApiAgentID(r.Context()))
}

func docURL(id string) string { return authService.FrontendBaseURL() + "/app/doc/" + id }

// listDocs is a page of the docs a token's owner can open. A seam for tests.
var listDocs = contentIOBusiness.ListDocs

// docError answers an error from the docs it came from. One that isn't the
// doc's, the caller's or the document service's own is logged, and answered
// as a failed read (503, try again: what every read answers for one)
// when read says the call only read, else as a failed write (500).
func docError(w http.ResponseWriter, r *http.Request, err error, read bool) {
	var refused *collab.OpError
	switch {
	case errors.Is(err, contentIOBusiness.ErrNotFound), errors.Is(err, collab.ErrDocInTrash), errors.Is(err, collab.ErrNoSuchDoc):
		helpers.WriteDocNotFound(w)
	case errors.Is(err, contentIOBusiness.ErrForbidden):
		refuse(w, http.StatusForbidden, "The token's owner can't change this doc, or add pages under it.")
	case errors.Is(err, contentIOBusiness.ErrEmpty):
		refuse(w, http.StatusBadRequest, "The markdown is empty: there is nothing to add.")
	case errors.Is(err, contentcodec.ErrTooDeep):
		refuse(w, http.StatusBadRequest, fmt.Sprintf("The markdown nests quotes or lists more than %d levels deep.", helpers.MaxMarkdownNesting))
	case errors.Is(err, contentcodec.ErrTooCostly):
		refuse(w, http.StatusBadRequest, "The markdown would take too long to read: a paragraph or a line holds too many brackets, backticks, asterisks or underscores that never close. Put such text in a code block, or split it.")
	case errors.Is(err, collab.ErrUnavailable), errors.Is(err, contentIOBusiness.ErrUnavailable):
		refuse(w, http.StatusServiceUnavailable, "The document service couldn't take the change just now. Try again shortly.")
	case errors.As(err, &refused):
		refuse(w, http.StatusUnprocessableEntity, "The document service refused the change: "+refused.Msg)
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/V1 %s %s: %+v", r.Method, helpers.RouteOf(r), err)
		if read {
			helpers.WriteRetryLater(w, "The doc couldn't be read just now.")
			return
		}
		refuse(w, http.StatusInternalServerError, "Something went wrong with the doc. Try again shortly.")
	}
}

// ListDocsV1 GET /v1/docs?scope=workspace|mine|shared&cursor=&limit= — scope
// docs:read. One page of the docs the token's owner can open, newest edit
// first: everyone's (workspace, the default), their own, or shared with them;
// an app's, only the docs it was added to.
func ListDocsV1(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	scope := q.Get("scope")
	switch scope {
	case "":
		scope = docDomain.ScopeWorkspace
	case docDomain.ScopeWorkspace, docDomain.ScopeMine, docDomain.ScopeShared:
	default:
		refuse(w, http.StatusBadRequest, "scope is workspace, mine or shared.")
		return
	}
	limit := helpers.LimitParam(q, apiDocPage, apiDocPageMax)
	offset, ok := helpers.ReadCursor(q.Get("cursor"))
	if !ok {
		refuse(w, http.StatusBadRequest, "That cursor isn't one this list gave.")
		return
	}
	listed, more, err := listDocs(r.Context(), apiSubject(r), scope, limit, offset)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/V1 ListDocsV1: %+v", err)
		helpers.WriteRetryLater(w, "The docs couldn't be listed just now.")
		return
	}
	// An app's page holds only the docs it was added to, so it can come up
	// short while next_cursor goes on.
	listed, _ = narrowed(r, listed, func(d contentIOBusiness.ListedDoc) []appBusiness.Place {
		return []appBusiness.Place{{Kind: "doc", ID: d.UUID}}
	})
	docs := make([]apiDoc, 0, len(listed))
	for _, d := range listed {
		docs = append(docs, apiDoc{ListedDoc: d, URL: docURL(d.UUID)})
	}
	next := ""
	if more {
		next = helpers.OffsetCursor(offset + limit)
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{"docs": docs, "next_cursor": next}})
}

// GetDocV1 GET /v1/docs/{id} — scope docs:read. The doc as Markdown, the same
// file as the app's Markdown export (its title as the first heading), read
// from the live doc, typing included. The workspace's export rule applies (on
// the route), and the read is recorded as an export made through the API.
func GetDocV1(w http.ResponseWriter, r *http.Request) {
	doc, md, err := contentIOBusiness.ReadForAPI(r.Context(), apiSubject(r), chi.URLParam(r, "id"))
	if err != nil {
		docError(w, r, err, true)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": apiDoc{ListedDoc: doc, URL: docURL(doc.UUID), Markdown: md}})
}

// docCreation is POST /v1/docs's body.
type docCreation struct {
	Title      string `json:"title"`
	Markdown   string `json:"markdown"`
	ParentUUID string `json:"parent_uuid"`
}

func readDocCreation(body io.Reader) (docCreation, error) {
	var d docCreation
	err := json.NewDecoder(body).Decode(&d)
	return d, err
}

// parentOf is the page a new doc goes under, read as CreateDocV1 reads it: the
// place its Reach names.
func parentOf(body []byte) string {
	d, err := readDocCreation(bytes.NewReader(body))
	if err != nil {
		return ""
	}
	return d.ParentUUID
}

// CreateDocV1 POST /v1/docs {title, markdown, parent_uuid} — scope docs:write.
// A new doc made from Markdown: under a page the token's owner can edit, where
// it takes that page's sharing, or at the top, where it is theirs alone. With
// no title, the heading 1 the Markdown starts with is its title.
func CreateDocV1(w http.ResponseWriter, r *http.Request) {
	body, err := readDocCreation(r.Body)
	if err != nil {
		refuse(w, http.StatusBadRequest, "Send a JSON object: title, markdown, and parent_uuid for a page to put it under.")
		return
	}
	made, skipped, err := contentIOBusiness.CreateFromMarkdown(r.Context(), apiSubject(r), body.Title, body.Markdown, body.ParentUUID)
	if err != nil {
		docError(w, r, err, false)
		return
	}
	audited(r, "create_doc", map[string]interface{}{"doc_uuid": made.UUID})
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": helpers.Envolope{
		"uuid": made.UUID, "title": made.Title, "parent_uuid": made.ParentUUID, "url": docURL(made.UUID), "skipped": helpers.NonNil(skipped),
	}})
}

// AppendToDocV1 POST /v1/docs/{id}/append {markdown} — scope docs:write.
// Markdown added to the end of the doc as one live edit, which everyone with
// the doc open sees at once; nothing already there changes.
func AppendToDocV1(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Markdown string `json:"markdown"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		refuse(w, http.StatusBadRequest, "Send a JSON object with the markdown to add.")
		return
	}
	id := chi.URLParam(r, "id")
	res, skipped, err := contentIOBusiness.AppendMarkdown(r.Context(), apiSubject(r), id, body.Markdown)
	if err != nil {
		docError(w, r, err, false)
		return
	}
	audited(r, "append_to_doc", map[string]interface{}{"doc_uuid": id, "blocks": res.Blocks})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{"uuid": id, "blocks": res.Blocks, "block_ids": res.BlockIDs, "skipped": helpers.NonNil(skipped)}})
}
