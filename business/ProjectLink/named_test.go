package business

import (
	"context"
	"errors"
	"testing"

	linkModel "github.com/akashc777/OneCamp/models/postgres/ProjectLink"
	"github.com/google/uuid"
)

// Links whose names their adder knows (a template's tables) are added in one
// write, in their order, named by their titles, and their pages aren't read.
// One address that isn't a web page's refuses them all before anything is
// written; the store's refusal (a duplicate, a full project) is answered.
func TestAddNamedAddsThemAllInOneWriteUnread(t *testing.T) {
	f := &fakes{}
	f.install(t)
	var writes [][]linkModel.Link
	var storeErr error
	create = func(_ context.Context, project uuid.UUID, links []linkModel.Link, _ uuid.UUID, most int) ([]linkModel.Link, error) {
		if most != MostLinks {
			t.Errorf("the cap: %d, want MostLinks", most)
		}
		writes = append(writes, links)
		if storeErr != nil {
			return nil, storeErr
		}
		out := make([]linkModel.Link, len(links))
		for i, l := range links {
			if l.ProjectUUID != project {
				t.Errorf("a link of another project: %v", l.ProjectUUID)
			}
			l.ID, l.Position = uuid.New(), i+1
			out[i] = l
		}
		return out, nil
	}
	project, by := uuid.New(), uuid.New()

	got, err := AddNamed(context.Background(), project, by, []Named{
		{URL: "https://work.example.com/app/tables/a1", Title: "Deals"},
		{URL: "https://work.example.com/app/tables/b2", Title: "  Contacts  "},
		{URL: "https://work.example.com/app/tables/c3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 || len(writes[0]) != 3 {
		t.Fatalf("writes %d, want one of three links", len(writes))
	}
	if f.unfurled != 0 || len(f.budgets) != 0 {
		t.Errorf("read %d pages, spent %v", f.unfurled, f.budgets)
	}
	titles := []string{got[0].Title, got[1].Title, got[2].Title}
	if titles[0] != "Deals" || titles[1] != "Contacts" || titles[2] != "work.example.com" || got[0].URL != "https://work.example.com/app/tables/a1" {
		t.Errorf("answered %v, %s", titles, got[0].URL)
	}

	writes = nil
	if _, err := AddNamed(context.Background(), project, by, []Named{{URL: "https://work.example.com/app/tables/a1", Title: "Deals"}, {URL: "ftp://files", Title: "Files"}}); !errors.Is(err, ErrInvalidURL) || len(writes) != 0 {
		t.Errorf("one bad address: %v, %d writes; want ErrInvalidURL and none", err, len(writes))
	}
	storeErr = ErrFull
	if _, err := AddNamed(context.Background(), project, by, []Named{{URL: "https://work.example.com/app/tables/a1", Title: "Deals"}}); !errors.Is(err, ErrFull) {
		t.Errorf("a full project: %v, want ErrFull", err)
	}
}
