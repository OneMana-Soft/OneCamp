//go:build integration
// +build integration

package integration_test

// Tables B5c-2 (airtable-07's branding): a form's cover and logo are pictures
// its managers uploaded for it (business/Form look.go). Over HTTP, on real
// stores: the maker puts up their own picture, and a visitor's page asks for it
// by the form's link, a 302 to the stored file, whose address a browser follows
// to the very bytes the maker put up; someone else's upload, or a
// file that isn't a picture, is refused in words and nothing changes; a member
// who can open the table but not change its forms is refused (403) and nothing
// is kept.
// Run: PKGS=./tests/integration/ integ_rel.sh <wt> <log> '^(TestAFormWearsOnlyPicturesItsManagersAdded)$'

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	formBusiness "github.com/akashc777/OneCamp/business/Form"
	formController "github.com/akashc777/OneCamp/controllers/Form"
	"github.com/akashc777/OneCamp/helpers"
	configModels "github.com/akashc777/OneCamp/models/postgres/Config"
	tableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

func TestAFormWearsOnlyPicturesItsManagersAdded(t *testing.T) {
	ctx := context.Background()
	store := integration.SetupMinio(t, "forms-look-test")
	env, dg := calendarStores(t)
	maker := seedCalendarPerson(t, env.PG, dg, "Maker")
	member := seedCalendarPerson(t, env.PG, dg, "Member")
	by := tableBusiness.Actor{UserID: maker.UserPostgresInfo.Id}
	table, err := tableBusiness.CreateTable(ctx, tableBusiness.TableInput{Name: "Leads", Visibility: tableModel.VisibilityWorkspace}, by)
	if err != nil {
		t.Fatalf("make the table: %v", err)
	}
	company, err := tableBusiness.CreateField(ctx, table.Id, tableBusiness.FieldInput{Name: "Company", Type: tableModel.FieldText, Position: 1}, by)
	if err != nil {
		t.Fatalf("make the column: %v", err)
	}
	// Files as the app's one upload keeps them for a form (src_key "form").
	upload := func(who uuid.UUID, name string) uuid.UUID {
		var id uuid.UUID
		key := "forms/look/" + uuid.NewString() + "-" + name
		if err := env.PG.QueryRowContext(ctx, `INSERT INTO attachments (obj_key, src_key, src_value, created_by) VALUES ($1, $2, 'new', $3) RETURNING id`,
			key, formBusiness.LookSource, who).Scan(&id); err != nil {
			t.Fatalf("upload %s: %v", name, err)
		}
		return id
	}
	// Pictures go up as the builder sends them: POST /user/uploadFile, src_key
	// "form", which anyone signed in may use (business/FileAccess).
	viaApp := func(who *userModels.UserInfo, name string) uuid.UUID {
		code, body := uploadAs(t, who.UserPostgresInfo, name, onePixelPNG, `{"src_key":"form","src_value":"new"}`)
		id, err := uuid.Parse(fmt.Sprint(body["object_uuid"]))
		if code != http.StatusOK || err != nil {
			t.Fatalf("the upload of %s: %d %v", name, code, body)
		}
		return id
	}
	cover := viaApp(maker, "cover.png")
	mark := viaApp(maker, "mark.png")
	theirs := viaApp(member, "logo.png")
	brief := upload(maker.UserPostgresInfo.Id, "brief.pdf")
	// What a visitor's page gets for a picture: the 302's address, fetched as a
	// browser follows it, must hold the bytes the maker uploaded. (A 302 to an
	// object that isn't there passed while this only read the address.)
	fetched := func(what string, rec *httptest.ResponseRecorder, want []byte) {
		t.Helper()
		where := rec.Header().Get("Location")
		if rec.Code != http.StatusFound || where == "" {
			t.Fatalf("%s by the link: %d %q, want a 302 to the stored picture", what, rec.Code, msgOf(rec))
		}
		res, err := http.Get(where)
		if err != nil {
			t.Fatalf("%s: following the 302: %v", what, err)
		}
		defer res.Body.Close()
		got, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK || !bytes.Equal(got, want) {
			t.Fatalf("%s: the 302's address answered %d with %d bytes, want the %d bytes stored", what, res.StatusCode, len(got), len(want))
		}
	}

	route := "/tables/" + table.Id.String() + "/forms"
	params := map[string]string{"id": table.Id.String()}
	in := formBusiness.Input{Title: "Vendor sign-up", Active: true, Cover: cover.String(), Fields: []formBusiness.Field{
		{Id: "company", Label: "Company", Type: formBusiness.ShortText, Required: true, MapsTo: formBusiness.MapColumn + company.Id.String()},
	}}
	save := func(who *userModels.UserInfo, in formBusiness.Input) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(in)
		return call(t, formController.SaveTableForm, http.MethodPost, route, who, string(raw), params)
	}
	stored := func() (cover, logo *uuid.UUID) {
		if err := env.PG.QueryRowContext(ctx, `SELECT cover_id, logo_id FROM project_forms WHERE target_table = $1`, table.Id).Scan(&cover, &logo); err != nil {
			t.Fatalf("read the form: %v", err)
		}
		return cover, logo
	}

	// A member who can open the table: refused in words, and nothing is kept.
	if rec := save(member, in); rec.Code != http.StatusForbidden || msgOf(rec) != "Only the table's owner or an admin can change its forms." {
		t.Fatalf("a member puts up a cover: %d %q, want 403", rec.Code, msgOf(rec))
	}
	var n int
	if err := env.PG.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_forms WHERE target_table = $1`, table.Id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after the member: %d forms (%v), want 0", n, err)
	}

	// Its maker: kept, and the visitor's page asks for it by the link.
	rec := save(maker, in)
	var saved struct {
		Data struct {
			Id    string `json:"id"`
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); rec.Code != http.StatusOK || err != nil || saved.Data.Token == "" {
		t.Fatalf("the maker saves: %d %q %v", rec.Code, msgOf(rec), err)
	}
	token := saved.Data.Token
	at := map[string]string{"token": token}
	code, pub := public(t, formController.GetPublicForm, http.MethodGet, "/public/form/"+token, "", at)
	var page struct {
		Cover string `json:"cover"`
		Logo  string `json:"logo"`
	}
	if err := json.Unmarshal(pub["data"], &page); code != http.StatusOK || err != nil || page.Cover != "/public/form/"+token+"/cover" || page.Logo != "" {
		t.Fatalf("the public page: %d %v cover %q logo %q; want its cover's path and no logo", code, err, page.Cover, page.Logo)
	}
	coverAt := map[string]string{"token": token, "kind": "cover"}
	fetched("the cover", publicCall(t, formController.GetPublicFormPicture, http.MethodGet, page.Cover, "", coverAt), onePixelPNG)
	if none := publicCall(t, formController.GetPublicFormPicture, http.MethodGet, "/public/form/"+token+"/logo", "", map[string]string{"token": token, "kind": "logo"}); none.Code != http.StatusNotFound || msgOf(none) != "This form has no logo." {
		t.Fatalf("no logo: %d %q, want 404", none.Code, msgOf(none))
	}

	// Closed, its page is gone, and its link still serves its cover, so its
	// builder shows it before the form opens.
	in.Id = saved.Data.Id
	in.Active = false
	if rec := save(maker, in); rec.Code != http.StatusOK {
		t.Fatalf("the maker closes it: %d %q", rec.Code, msgOf(rec))
	}
	if code, _ := public(t, formController.GetPublicForm, http.MethodGet, "/public/form/"+token, "", at); code == http.StatusOK {
		t.Fatal("a closed form's page still opens")
	}
	if pic := publicCall(t, formController.GetPublicFormPicture, http.MethodGet, page.Cover, "", coverAt); pic.Code != http.StatusFound {
		t.Fatalf("a closed form's cover by its link: %d %q, want 302", pic.Code, msgOf(pic))
	}
	in.Active = true

	// Someone else's upload, or a file that isn't a picture: refused in words, the form as it was.
	in.Id = saved.Data.Id
	for name, id := range map[string]uuid.UUID{"the member's upload": theirs, "a PDF": brief} {
		in.Logo = id.String()
		if rec := save(maker, in); rec.Code != http.StatusBadRequest || msgOf(rec) != "Add a PNG, JPEG, WebP or GIF picture as the form's logo." {
			t.Fatalf("%s as the logo: %d %q, want 400 in words", name, rec.Code, msgOf(rec))
		}
		if c, l := stored(); c == nil || *c != cover || l != nil {
			t.Fatalf("after %s: cover %v logo %v, want the cover kept and no logo", name, c, l)
		}
	}
	// Its maker's own logo: kept, and its link serves the picture itself.
	in.Logo = mark.String()
	if rec := save(maker, in); rec.Code != http.StatusOK {
		t.Fatalf("the maker's own logo: %d %q", rec.Code, msgOf(rec))
	}
	if _, l := stored(); l == nil || *l != mark {
		t.Fatalf("the maker's own logo kept as %v, want %v", l, mark)
	}
	logoAt := map[string]string{"token": token, "kind": "logo"}
	fetched("the form's own logo", publicCall(t, formController.GetPublicFormPicture, http.MethodGet, "/public/form/"+token+"/logo", "", logoAt), onePixelPNG)
	// Its logo taken off, it keeps its cover; the cover taken off, the page asks for none.
	in.Logo = ""
	if rec := save(maker, in); rec.Code != http.StatusOK {
		t.Fatalf("the logo taken off: %d %q", rec.Code, msgOf(rec))
	}
	if c, l := stored(); c == nil || *c != cover || l != nil {
		t.Fatalf("the logo taken off: cover %v logo %v, want the cover kept and no logo", c, l)
	}
	// The workspace's logo, kept as the admin's email settings keep it (its
	// object, and its whole name in the config): a form with no logo of its own
	// wears it, and the form's link serves those very bytes.
	workspace := append(append([]byte(nil), onePixelPNG...), "workspace"...)
	workspaceName := "systemFileUpload/email_logo_" + uuid.NewString() + ".png"
	if _, err := store.PutObject(ctx, helpers.UserUploadBucket(), workspaceName, bytes.NewReader(workspace), int64(len(workspace)), minio.PutObjectOptions{ContentType: "image/png"}); err != nil {
		t.Fatalf("store the workspace's logo: %v", err)
	}
	if err := configModels.UpsertConfig(configModels.EmailLogoKey, workspaceName); err != nil {
		t.Fatalf("keep the workspace's logo: %v", err)
	}
	t.Cleanup(func() { _ = configModels.UpsertConfig(configModels.EmailLogoKey, "") })
	if _, pub := public(t, formController.GetPublicForm, http.MethodGet, "/public/form/"+token, "", at); !strings.Contains(string(pub["data"]), `"/public/form/`+token+`/logo"`) {
		t.Fatalf("a form with no logo of its own, in a workspace with one: its page asks for no logo: %s", pub["data"])
	}
	fetched("the workspace's logo, on a form with none of its own", publicCall(t, formController.GetPublicFormPicture, http.MethodGet, "/public/form/"+token+"/logo", "", logoAt), workspace)
	in.Cover = ""
	if rec := save(maker, in); rec.Code != http.StatusOK {
		t.Fatalf("the cover taken off: %d %q", rec.Code, msgOf(rec))
	}
	if c, _ := stored(); c != nil {
		t.Fatalf("the cover taken off is still %v", c)
	}
	if _, pub := public(t, formController.GetPublicForm, http.MethodGet, "/public/form/"+token, "", at); strings.Contains(string(pub["data"]), `"cover"`) {
		t.Fatalf("a form with no cover still serves one: %s", pub["data"])
	}
}
