package business

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	taskViewModel "github.com/akashc777/OneCamp/models/postgres/TaskView"
	"github.com/google/uuid"
)

// Widgets put on together (AddWidgets, and AddWidget as a batch of one) have
// their saved views checked together: one read through the access rule and
// one of the live views for the whole batch, none for a batch over no view,
// and one view the keeper can't put there refuses the batch before anything
// is kept.
func TestABatchOfWidgetsChecksItsViewsInOneRead(t *testing.T) {
	views := map[string]taskViewModel.TaskView{}
	decisions := map[string]accessBusiness.Decision{}
	add := func(d accessBusiness.Decision) string {
		id := uuid.New()
		views[id.String()] = taskViewModel.TaskView{Id: id, Scope: "mine", State: json.RawMessage(`{"filters":[]}`)}
		decisions[id.String()] = d
		return id.String()
	}
	own := accessBusiness.Decision{Level: accessBusiness.LevelFull, Was: accessBusiness.LevelFull, Owner: true}
	mine, alsoMine, others := add(own), add(own), add(accessBusiness.Decision{})
	decides, lives := 0, 0
	defer swapViewSeams(
		func(_ context.Context, _ accessBusiness.Subject, res []accessBusiness.Resource) (map[accessBusiness.Resource]accessBusiness.Decision, error) {
			decides++
			out := map[accessBusiness.Resource]accessBusiness.Decision{}
			for _, r := range res {
				if d, ok := decisions[r.UUID]; ok {
					out[r] = d
				}
			}
			return out, nil
		},
		func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]taskViewModel.TaskView, error) {
			lives++
			out := map[uuid.UUID]taskViewModel.TaskView{}
			for _, id := range ids {
				if v, ok := views[id.String()]; ok {
					out[id] = v
				}
			}
			return out, nil
		})()

	keeper := uuid.New()
	d := Dashboard{ID: uuid.NewString(), Scope: ScopeWorkspace}
	over := func(view string) Widget {
		return Widget{Kind: KindThroughput, Query: Query{View: view}, X: 0, Y: 0, W: 6, H: 2}
	}
	reads := func(what string, wantDecides, wantLives int) {
		t.Helper()
		if decides != wantDecides || lives != wantLives {
			t.Errorf("%s read the views %d and %d times, want %d and %d", what, decides, lives, wantDecides, wantLives)
		}
		decides, lives = 0, 0
	}

	if err := checkViews(context.Background(), d, []Widget{over(mine), over(""), over(alsoMine)}, keeper); err != nil {
		t.Errorf("two of the keeper's own views and a widget over none: %v", err)
	}
	reads("a batch over two views", 1, 1)

	if err := checkViews(context.Background(), d, []Widget{over(""), over("")}, keeper); err != nil {
		t.Errorf("a batch over no view: %v", err)
	}
	reads("a batch over no view", 0, 0)

	var refusal *Refusal
	if err := checkViews(context.Background(), d, []Widget{over(mine), over(others)}, keeper); !errors.As(err, &refusal) {
		t.Errorf("a batch with someone else's view = %v, want a Refusal", err)
	}
	reads("a batch with someone else's view", 1, 1)

	// AddWidgets refuses that batch before it keeps anything: the refusal
	// comes back, not the store's (no store is connected here).
	if made, err := AddWidgets(context.Background(), d, []Widget{over(mine), over(others)}, keeper); !errors.As(err, &refusal) || made != nil {
		t.Errorf("AddWidgets with someone else's view = %v, %v, want no widgets and a Refusal", made, err)
	}
	reads("AddWidgets", 1, 1)
	if _, err := AddWidget(context.Background(), d, over(others), keeper); !errors.As(err, &refusal) {
		t.Errorf("AddWidget over someone else's view = %v, want a Refusal", err)
	}
	reads("AddWidget", 1, 1)
}
