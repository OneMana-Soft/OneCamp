package docController

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// levelsRules answers each doc's level by its uuid; none for one it doesn't
// name.
type levelsRules struct {
	accessBusiness.Resolver
	levels map[string]accessBusiness.Level
}

func (l levelsRules) Resolve(_ context.Context, _ accessBusiness.Subject, res accessBusiness.Resource) (accessBusiness.Decision, error) {
	lv, ok := l.levels[res.UUID]
	if !ok {
		return accessBusiness.Decision{}, accessBusiness.ErrNotFound
	}
	return accessBusiness.Decision{Level: lv}, nil
}

// Colouring (docs F1, the F1a/F1b contract 6): at most 200 docs, each one the
// person may edit, or nothing is coloured; a camp hue, or null to clear; the
// answer is how many changed.
func TestDocsAreColouredTogetherOnlyWhereThePersonMayEditEach(t *testing.T) {
	a, b := "5f0c4a2e-8a63-4f3e-9d2b-1c7a9e0b3d41", "6a1d5b3f-9b74-4a4f-8e3c-2d8b0f1c4e52"
	prevRules, prevSet := accessBusiness.Rules, setColors
	t.Cleanup(func() { accessBusiness.Rules, setColors = prevRules, prevSet })
	var wrote [][]string
	var colours []string
	setColors = func(_ context.Context, ids []string, color string, _ business.Actor) (int, error) {
		wrote = append(wrote, ids)
		colours = append(colours, color)
		return len(ids), nil
	}
	ask := func(body string) (int, map[string]any) {
		r := httptest.NewRequest(http.MethodPost, "/doc/color", strings.NewReader(body))
		who := userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "b0b00000-0000-4000-8000-000000000002"}}
		r = r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, who))
		w := httptest.NewRecorder()
		SetDocColor(w, r)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	accessBusiness.Rules = levelsRules{levels: map[string]accessBusiness.Level{a: accessBusiness.LevelEdit, b: accessBusiness.LevelEdit}}
	code, out := ask(`{"doc_uuids":["` + a + `","` + b + `","` + a + `"],"color":"moss"}`)
	data, _ := out["data"].(map[string]any)
	if code != http.StatusOK || data["count"] != float64(2) || len(wrote) != 1 || len(wrote[0]) != 2 || colours[0] != "moss" {
		t.Fatalf("two docs coloured: %d %v; wrote %v %v", code, out, wrote, colours)
	}
	if code, _ := ask(`{"doc_uuids":["` + a + `"],"color":null}`); code != http.StatusOK || colours[1] != "" {
		t.Errorf("a colour cleared: %d %v", code, colours)
	}

	wrote = nil
	for name, body := range map[string]string{
		"not a hue":    `{"doc_uuids":["` + a + `"],"color":"red"}`,
		"an empty one": `{"doc_uuids":["` + a + `"],"color":""}`,
		"no docs":      `{"doc_uuids":[],"color":"sky"}`,
		"not a doc id": `{"doc_uuids":["x"],"color":"sky"}`,
	} {
		if code, out := ask(body); code != http.StatusBadRequest {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	ids := make([]string, 201)
	for i := range ids {
		ids[i] = fmt.Sprintf(`"00000000-0000-4000-8000-%012d"`, i)
	}
	if code, out := ask(`{"doc_uuids":[` + strings.Join(ids, ",") + `],"color":"sky"}`); code != http.StatusBadRequest {
		t.Errorf("201 docs: %d %v", code, out)
	}
	accessBusiness.Rules = levelsRules{levels: map[string]accessBusiness.Level{a: accessBusiness.LevelEdit, b: accessBusiness.LevelView}}
	if code, out := ask(`{"doc_uuids":["` + a + `","` + b + `"],"color":"sky"}`); code != http.StatusForbidden {
		t.Errorf("one the person may only view: %d %v", code, out)
	}
	accessBusiness.Rules = levelsRules{levels: map[string]accessBusiness.Level{a: accessBusiness.LevelEdit}}
	if code, out := ask(`{"doc_uuids":["` + a + `","` + b + `"],"color":"sky"}`); code != http.StatusNotFound {
		t.Errorf("one that isn't there: %d %v", code, out)
	}
	if len(wrote) != 0 {
		t.Errorf("a refused colouring wrote %v", wrote)
	}
}
