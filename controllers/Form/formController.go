package controllers

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	formBusiness "github.com/akashc777/OneCamp/business/Form"
	"github.com/akashc777/OneCamp/business/Formula"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	formModel "github.com/akashc777/OneCamp/models/postgres/Form"
	"github.com/go-chi/chi/v5"
)

// Intake forms: see business/Form. A project's members see its forms; its
// admins make and change them; anyone with the link fills one in.

const denied = "Only the project's admins can change its forms."

// inputLimit is the most a form's saved shape, or a visitor's answers, may
// take of a request's body.
const inputLimit = 64 << 10

func write(w http.ResponseWriter, r *http.Request, where string, err error) {
	var fe *formBusiness.FormError
	switch {
	case errors.As(err, &fe):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": fe.Error()})
	case errors.Is(err, formBusiness.ErrNoForm):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "This form isn't available."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Form/%s err: %+v", where, err)
		helpers.WriteRetryLater(w, "Something went wrong.")
	}
}

// ListForms. GET /project/{p}/forms
func ListForms(w http.ResponseWriter, r *http.Request) {
	id, _, standing, ok := projectaccess.RequireStanding(w, r, workAccess.NeedView, denied)
	if !ok {
		return
	}
	forms, err := formModel.List(r.Context(), id)
	if err != nil {
		write(w, r, "ListForms", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"forms": forms, "can_edit": standing.Can(workAccess.NeedManage)}})
}

// SaveForm creates or updates a form. POST /project/{p}/forms
func SaveForm(w http.ResponseWriter, r *http.Request) {
	id, _, ok := projectaccess.Require(w, r, workAccess.NeedManage, denied)
	if !ok {
		return
	}
	var in formBusiness.Input
	if !httpx.ReadJSON(w, r, inputLimit, &in) {
		return
	}
	userInfo, signedIn := httpx.MustUser(w, r)
	if !signedIn {
		return
	}
	f, err := formBusiness.Save(r.Context(), id, in, userInfo.UserPostgresInfo.Id)
	if err != nil {
		write(w, r, "SaveForm", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": f})
}

// DeleteForm; tasks it made stay. POST /project/{p}/forms/{form_id}/delete
func DeleteForm(w http.ResponseWriter, r *http.Request) {
	id, _, ok := projectaccess.Require(w, r, workAccess.NeedManage, denied)
	if !ok {
		return
	}
	formID, ok := helpers.UUIDParam(w, r, "form_id")
	if !ok {
		return
	}
	userInfo, signedIn := httpx.MustUser(w, r)
	if !signedIn {
		return
	}
	deleted, err := formModel.Delete(r.Context(), id, formID, userInfo.UserPostgresInfo.Id)
	if err != nil {
		write(w, r, "DeleteForm", err)
		return
	}
	writeDeleted(w, deleted)
}

// writeDeleted answers a form's delete: 404 when there was no such form.
func writeDeleted(w http.ResponseWriter, deleted bool) {
	if !deleted {
		helpers.WriteNotFound(w, "Form not found")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Deleted"})
}

// FormSubmissions is a form's latest 50 submissions, with their answers and
// the tasks they made: for the project's admins, as answers can carry a
// visitor's email. GET /project/{p}/forms/{form_id}/submissions
func FormSubmissions(w http.ResponseWriter, r *http.Request) {
	id, _, ok := projectaccess.Require(w, r, workAccess.NeedManage, denied)
	if !ok {
		return
	}
	formID, ok := helpers.UUIDParam(w, r, "form_id")
	if !ok {
		return
	}
	f, err := formModel.Get(r.Context(), id, formID)
	if err != nil {
		write(w, r, "FormSubmissions", err)
		return
	}
	writeSubmissions(w, r, "FormSubmissions", f)
}

// writeSubmissions answers form f's latest 50 submissions, with their answers
// and what each made, and how many it has had: 404 when there's no form.
func writeSubmissions(w http.ResponseWriter, r *http.Request, where string, f *formModel.Form) {
	if f == nil {
		helpers.WriteNotFound(w, "Form not found")
		return
	}
	subs, err := formModel.Latest(r.Context(), f.Id, 50)
	if err != nil {
		write(w, r, where, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"submissions": subs, "total": f.Submissions}})
}

// GetPublicForm is a form's questions, for anyone with the link.
// GET /public/form/{token}
func GetPublicForm(w http.ResponseWriter, r *http.Request) {
	f, err := formBusiness.GetPublic(r.Context(), chi.URLParam(r, "token"))
	if err != nil {
		write(w, r, "GetPublicForm", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": f})
}

// GetPublicFormPicture is a form's cover or logo for its page, by its link:
// a 302 to the stored picture, the workspace's logo for a form with none of
// its own, and 404 when there is none (tables B5c-2).
// GET /public/form/{token}/{kind:cover|logo}
func GetPublicFormPicture(w http.ResponseWriter, r *http.Request) {
	kind := chi.URLParam(r, "kind")
	key, err := formBusiness.LookPicture(r.Context(), chi.URLParam(r, "token"), kind)
	if errors.Is(err, formBusiness.ErrNoPicture) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "This form has no " + kind + "."})
		return
	}
	if err != nil {
		write(w, r, "GetPublicFormPicture", err)
		return
	}
	httpx.RedirectToStored(w, r, key, kind)
}

// SubmitPublicForm turns answers into a task. POST /public/form/{token}
func SubmitPublicForm(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Answers map[string]any `json:"answers"`
		// Website is a field people never see: a form that fills it is a bot.
		Website string `json:"website"`
		// How the visitor writes numbers ("decimal", "group"), as the tables'
		// requests send them: a number answer is read in them.
		formula.NumberSigns
		// The visitor's zone, where a date that sets the due date is a day.
		TZ string `json:"tz"`
		// Claim is what the page holds for the files it uploaded.
		Claim string `json:"claim"`
	}
	if !httpx.ReadJSON(w, r, inputLimit, &in) {
		return
	}
	if in.Website != "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't send that."})
		return
	}
	if err := formBusiness.Submit(r.Context(), chi.URLParam(r, "token"), in.Answers, in.NumberSigns, in.TZ, in.Claim, time.Now()); err != nil {
		write(w, r, "SubmitPublicForm", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Sent"})
}

// UploadPublicFormFile keeps a file a visitor adds to one of a form's file
// questions, until the form is sent with it: the multipart "file", with
// "question" and, once the page holds one, "claim". It answers the file and
// the claim (business/Form UploadVisitorFile). POST /public/form/{token}/file
func UploadPublicFormFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	mb := formBusiness.FileLimitMB()
	// The file and the form's few fields; the router's BodyLimit refuses a
	// body past the largest limit before it is read.
	r.Body = http.MaxBytesReader(w, r.Body, mb<<20+64<<10)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			helpers.WriteJSON(w, http.StatusRequestEntityTooLarge, helpers.Envolope{"msg": fmt.Sprintf("Files can be up to %d MB.", mb)})
			return
		}
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that file."})
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	file, header, err := r.FormFile("file")
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Add a file."})
		return
	}
	defer func() { _ = file.Close() }()
	sent, err := formBusiness.UploadVisitorFile(ctx, chi.URLParam(r, "token"), r.FormValue("claim"), r.FormValue("question"),
		file, header.Size, header.Filename, time.Now())
	var fe *formBusiness.FormError
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": sent})
	case errors.As(err, &fe), errors.Is(err, formBusiness.ErrNoForm):
		write(w, r, "UploadPublicFormFile", err)
	default:
		helpers.WriteUploadFailure(ctx, w, err, "The file couldn't be kept. Try again in a while.")
	}
}
