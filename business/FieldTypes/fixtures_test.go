package business

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	formula "github.com/akashc777/OneCamp/business/Formula"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// The golden cases both registries are held to (testdata/fieldtypes, the
// same file in the web's repository, read by lib/fieldTypes/fixtures.test.ts).
type fixture struct {
	Options []map[string]any `json:"options"`
	Cases   []struct {
		Name   string               `json:"name"`
		Kind   string               `json:"kind"`
		Config map[string]any       `json:"config"`
		In     any                  `json:"in"`
		Signs  *formula.NumberSigns `json:"signs"`
		Want   any                  `json:"want"`
		Error  bool                 `json:"error"`
	} `json:"cases"`
}

func readFixture(t *testing.T) fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fieldtypes", "coerce.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) < 40 {
		t.Fatalf("only %d cases: the fixture isn't the one both sides read", len(f.Cases))
	}
	return f
}

// Every writer's value is stored as the golden cases say: what each kind
// stores, from any writer, and what it refuses.
func TestEveryKindStoresWhatTheFixturesSay(t *testing.T) {
	fx := readFixture(t)
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			cfg := c.Config
			if cfg == nil {
				cfg = map[string]any{}
			}
			if c.Kind == model.FieldSelect || c.Kind == model.FieldMultiSelect {
				cfg["options"] = fx.Options
			}
			b, _ := json.Marshal(cfg)
			f := &model.Field{Id: uuid.New(), Name: "Field", Type: c.Kind, Config: string(b)}
			in := Input{}
			if c.Signs != nil {
				in.Signs = *c.Signs
			}
			got, err := Kinds.Check([]*model.Field{f}, map[string]any{f.Id.String(): c.In}, in)
			if c.Error {
				if err == nil {
					t.Fatalf("stored %#v, want it refused", got[f.Id.String()])
				}
				cells, ok := err.(CellErrors)
				if !ok || len(cells) != 1 || cells[0].Field != f.Id.String() {
					t.Errorf("the refusal %v doesn't name the field", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			// As stored: through JSON, as a row's cells are.
			var want, have any
			wb, _ := json.Marshal(c.Want)
			hb, _ := json.Marshal(got[f.Id.String()])
			_ = json.Unmarshal(wb, &want)
			_ = json.Unmarshal(hb, &have)
			if !reflect.DeepEqual(want, have) {
				t.Errorf("stored %s, want %s", hb, wb)
			}
		})
	}
}
