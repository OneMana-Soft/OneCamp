//go:build integration
// +build integration

package integration_test

// VERIFY-LEAD-2's V-13: a table, or a project, keeps at most 20 forms however
// many are made at once. The count and the insert are one step under a lock
// (formModel.CreateWithin, on pgexec.Capped); counted apart, every save that
// counted before the others inserted saw room for the last place.
// Run: PKGS=./tests/integration/ integ_rel.sh <wt> <log> '^(TestTwentyFormsAtMostWhenManyAreMadeAtOnce)$'

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"

	tableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	formBusiness "github.com/akashc777/OneCamp/business/Form"
	formController "github.com/akashc777/OneCamp/controllers/Form"
	tableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	formModel "github.com/akashc777/OneCamp/models/postgres/Form"
)

// A table with 18 forms of its own takes twelve new ones posted at once over
// HTTP: two are made, ten are refused in words, and it lists 20. A project's
// forms are capped the same way, under the project's own lock.
func TestTwentyFormsAtMostWhenManyAreMadeAtOnce(t *testing.T) {
	const tries = 12
	ctx := context.Background()
	env, dg := calendarStores(t)
	maker := seedCalendarPerson(t, env.PG, dg, "Maker")
	by := tableBusiness.Actor{UserID: maker.UserPostgresInfo.Id}
	table, err := tableBusiness.CreateTable(ctx, tableBusiness.TableInput{Name: "Leads", Visibility: tableModel.VisibilityWorkspace}, by)
	if err != nil {
		t.Fatalf("make the table: %v", err)
	}
	company, err := tableBusiness.CreateField(ctx, table.Id, tableBusiness.FieldInput{Name: "Company", Type: tableModel.FieldText, Required: true, Position: 1}, by)
	if err != nil {
		t.Fatalf("its column: %v", err)
	}
	route := "/tables/" + table.Id.String() + "/forms"
	params := map[string]string{"id": table.Id.String()}
	save := func(title string) (int, string) {
		raw, _ := json.Marshal(formBusiness.Input{Title: title, Active: true, Fields: []formBusiness.Field{
			{Id: "company", Label: "Company", Type: formBusiness.ShortText, Required: true, MapsTo: formBusiness.MapColumn + company.Id.String()}}})
		rec := call(t, formController.SaveTableForm, http.MethodPost, route, maker, string(raw), params)
		return rec.Code, msgOf(rec)
	}
	for i := 0; i < 18; i++ {
		if code, msg := save(fmt.Sprintf("Sign-up %d", i)); code != http.StatusOK {
			t.Fatalf("form %d: %d %q", i, code, msg)
		}
	}

	// Twelve at once, released together.
	start := make(chan struct{})
	codes, msgs := make([]int, tries), make([]string, tries)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i], msgs[i] = save(fmt.Sprintf("Rush %d", i))
		}(i)
	}
	close(start)
	wg.Wait()
	made, refused := 0, 0
	for i, code := range codes {
		switch {
		case code == http.StatusOK:
			made++
		case code == http.StatusBadRequest && msgs[i] == "This table has 20 forms. Delete one to make another.":
			refused++
		default:
			t.Errorf("save %d: %d %q", i, code, msgs[i])
		}
	}
	if made != 2 || refused != tries-2 {
		t.Errorf("at once: %d made and %d refused, want 2 and %d", made, refused, tries-2)
	}
	rec := call(t, formController.ListTableForms, http.MethodGet, route, maker, "", params)
	var listed struct {
		Data struct {
			Forms []formModel.Form `json:"forms"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); rec.Code != http.StatusOK || err != nil || len(listed.Data.Forms) != 20 {
		t.Errorf("the table lists %d forms (%d %v), want 20", len(listed.Data.Forms), rec.Code, err)
	}

	// A project's: 18 there, twelve made at once, two kept.
	project := uuid.New()
	form := func(title string) formModel.Form {
		return formModel.Form{ProjectUUID: project, Token: uuid.NewString(), Title: title, Fields: json.RawMessage(`[]`), Active: true, CreatedBy: maker.UserPostgresInfo.Id}
	}
	for i := 0; i < 18; i++ {
		if f, _, err := formModel.CreateWithin(ctx, form(fmt.Sprintf("Request %d", i)), 20); f == nil || err != nil {
			t.Fatalf("project form %d: %v", i, err)
		}
	}
	start = make(chan struct{})
	kept := make([]bool, tries)
	errs := make([]error, tries)
	for i := range kept {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			f, _, err := formModel.CreateWithin(ctx, form(fmt.Sprintf("Rush %d", i)), 20)
			kept[i], errs[i] = f != nil, err
		}(i)
	}
	close(start)
	wg.Wait()
	n := 0
	for i, k := range kept {
		if errs[i] != nil {
			t.Errorf("project save %d: %v", i, errs[i])
		}
		if k {
			n++
		}
	}
	if count, err := formModel.Count(ctx, project); n != 2 || count != 20 || err != nil {
		t.Errorf("a project's at once: %d kept, %d there (%v), want 2 and 20", n, count, err)
	}
}
