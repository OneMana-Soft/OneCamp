//go:build integration
// +build integration

package integration_test

// Tables B5c (main, 28 Oct): a link hides a shown question only where the
// form's managers mark it ("A link can fill and hide this"). The mark is kept
// with the table's form and served with its public page; only someone who
// manages the table saves its forms, so a member who can open the table can't
// mark a question.
// Run: PKGS=./tests/integration/ integ_rel.sh <wt> <log> '^(TestOnlyAFormsManagersLetALinkHideAQuestion)$'

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	tableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	formBusiness "github.com/akashc777/OneCamp/business/Form"
	formController "github.com/akashc777/OneCamp/controllers/Form"
	tableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

func TestOnlyAFormsManagersLetALinkHideAQuestion(t *testing.T) {
	ctx := context.Background()
	env, dg := calendarStores(t)
	maker := seedCalendarPerson(t, env.PG, dg, "Maker")
	member := seedCalendarPerson(t, env.PG, dg, "Member")
	by := tableBusiness.Actor{UserID: maker.UserPostgresInfo.Id}
	table, err := tableBusiness.CreateTable(ctx, tableBusiness.TableInput{Name: "Leads", Visibility: tableModel.VisibilityWorkspace}, by)
	if err != nil {
		t.Fatalf("make the table: %v", err)
	}
	column := func(name string, at float64) string {
		f, err := tableBusiness.CreateField(ctx, table.Id, tableBusiness.FieldInput{Name: name, Type: tableModel.FieldText, Position: at}, by)
		if err != nil {
			t.Fatalf("its column %s: %v", name, err)
		}
		return formBusiness.MapColumn + f.Id.String()
	}
	company, source := column("Company", 1), column("Source", 2)
	route := "/tables/" + table.Id.String() + "/forms"
	params := map[string]string{"id": table.Id.String()}
	raw, _ := json.Marshal(formBusiness.Input{Title: "Vendor sign-up", Active: true, Fields: []formBusiness.Field{
		{Id: "company", Label: "Company", Type: formBusiness.ShortText, Required: true, MapsTo: company},
		{Id: "src", Label: "Source", Type: formBusiness.ShortText, LinkHides: true, MapsTo: source},
	}})
	listed := func() int {
		rec := call(t, formController.ListTableForms, http.MethodGet, route, maker, "", params)
		var out struct {
			Data struct {
				Forms []json.RawMessage `json:"forms"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); rec.Code != http.StatusOK || err != nil {
			t.Fatalf("list: %d %v", rec.Code, err)
		}
		return len(out.Data.Forms)
	}

	// A member who can open the table: refused in words, and nothing is kept.
	if rec := call(t, formController.SaveTableForm, http.MethodPost, route, member, string(raw), params); rec.Code != http.StatusForbidden || msgOf(rec) != "Only the table's owner or an admin can change its forms." {
		t.Fatalf("a member marks a question: %d %q, want 403", rec.Code, msgOf(rec))
	}
	if n := listed(); n != 0 {
		t.Fatalf("after the member's try the table lists %d forms, want 0", n)
	}

	// Its maker: kept, and served with the public page, the unmarked question without it.
	rec := call(t, formController.SaveTableForm, http.MethodPost, route, maker, string(raw), params)
	var saved struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); rec.Code != http.StatusOK || err != nil || saved.Data.Token == "" {
		t.Fatalf("the maker saves: %d %q %v", rec.Code, msgOf(rec), err)
	}
	// The public page is read by nobody signed in (calendar_attendance_test's public).
	code, pub := public(t, formController.GetPublicForm, http.MethodGet, "/public/form/"+saved.Data.Token, "", map[string]string{"token": saved.Data.Token})
	var page struct {
		Fields []formBusiness.Field `json:"fields"`
	}
	if err := json.Unmarshal(pub["data"], &page); code != http.StatusOK || err != nil || len(page.Fields) != 2 {
		t.Fatalf("the public page: %d %v %s", code, err, pub["data"])
	}
	if page.Fields[0].LinkHides || !page.Fields[1].LinkHides {
		t.Errorf("served marks: Company %v, Source %v; want false, true", page.Fields[0].LinkHides, page.Fields[1].LinkHides)
	}
}
