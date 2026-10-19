package docController

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/helpers"
)

// A docs path refusing a hosted doc (a table row's page) tells someone the
// row is open to where it is, so the doc page can take them there; someone
// it was there for before its row went to the trash that it was deleted; and
// anyone else exactly what a doc that isn't there gets, since the place names
// the table. A doc with no host is left to the path's own refusal.
func TestARefusedHostedDocIsPlacedOnlyForItsHostsReaders(t *testing.T) {
	oldHost, oldDecide, oldPlace := hostOfDoc, decideDoc, hostedPlace
	t.Cleanup(func() { hostOfDoc, decideDoc, hostedPlace = oldHost, oldDecide, oldPlace })
	row := business.Host{Kind: "table_row", UUID: "8e2b4d6f-1a3c-4e5f-8b7a-9c0d1e2f3a4b"}
	var host business.Host
	var hostErr error
	hostOfDoc = func(context.Context, string) (business.Host, error) { return host, hostErr }
	var d accessBusiness.Decision
	decideDoc = func(context.Context, accessBusiness.Subject, string) (accessBusiness.Decision, error) { return d, nil }
	hostedPlace = func(context.Context, business.Host) (business.HostPlace, error) {
		return business.HostPlace{Belongs: "This page belongs to a row of Deals. Change it there.", Path: "/app/tables/t?row=r"}, nil
	}
	answer := func() (bool, *httptest.ResponseRecorder) {
		rec := httptest.NewRecorder()
		return refuseHosted(context.Background(), rec, accessBusiness.Subject{UserUUID: "u"}, "d"), rec
	}
	missing := httptest.NewRecorder()
	helpers.WriteDocNotFound(missing)

	// No host: the path refuses it as any doc.
	if answered, rec := answer(); answered || rec.Body.Len() != 0 {
		t.Fatalf("a doc with no host was answered: %d %s", rec.Code, rec.Body)
	}
	host = row

	d = accessBusiness.Decision{Hosted: true, Level: accessBusiness.LevelEdit}
	answered, rec := answer()
	var said map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &said); err != nil || !answered || rec.Code != http.StatusForbidden || len(said) != 3 ||
		said["code"] != "hosted" || said["msg"] != "This page belongs to a row of Deals. Change it there." || said["path"] != "/app/tables/t?row=r" {
		t.Errorf("someone who may open the row: %d %s", rec.Code, rec.Body)
	}

	for name, stranger := range map[string]accessBusiness.Decision{
		"someone the row isn't open to":           {Hosted: true},
		"someone the row was never open to, gone": {Hosted: true, Deleted: true},
	} {
		d = stranger
		answered, rec := answer()
		if !answered || rec.Code != missing.Code || rec.Body.String() != missing.Body.String() {
			t.Errorf("%s: %d %q, want exactly a missing doc's %d %q", name, rec.Code, rec.Body, missing.Code, missing.Body)
		}
	}

	d = accessBusiness.Decision{Hosted: true, Deleted: true, Was: accessBusiness.LevelView}
	gone := httptest.NewRecorder()
	docGone(gone, true)
	if _, rec := answer(); rec.Code != gone.Code || rec.Body.String() != gone.Body.String() {
		t.Errorf("someone the row was open to before its trash: %d %s", rec.Code, rec.Body)
	}

	hostErr = errors.New("the graph didn't answer")
	if answered, rec := answer(); !answered || rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a host that couldn't be read: %v %d", answered, rec.Code)
	}
	hostErr = business.ErrNotFound
	if answered, _ := answer(); answered {
		t.Error("a doc that isn't there was answered as a hosted one")
	}
}
