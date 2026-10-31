package docController

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	publishBusiness "github.com/akashc777/OneCamp/business/Publish"
	treeBusiness "github.com/akashc777/OneCamp/business/Tree"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A member's row names who made the doc's last change beside when (docs F1,
// DOCS-LEAD-2's ruling: a row's "Maya · 3h ago"), as a list row names its
// owner: a tree row and a list row alike, under updated_by. A doc nobody is
// named for (before anyone changed it, or a node made a moment ago, which
// holds the editor's graph uid only) says no one, and the owner never stands
// in for the editor.
func TestARowNamesWhoMadeItsLastChange(t *testing.T) {
	maya := &dgraphStruct.DgraphUser{Uid: "0x9", Uuid: "c0c00000-0000-4000-8000-000000000003", UserName: "Maya", UserFullName: "Maya Chen"}
	owner := &dgraphStruct.DgraphUser{Uid: "0x8", Uuid: "d0d00000-0000-4000-8000-000000000004", UserName: "Leo"}
	raw, err := json.Marshal(treeNodes([]treeBusiness.Node{
		{Kind: "doc", UUID: "a", Title: "Plan", Data: &dgraphStruct.DgraphDoc{Uuid: "a", UpdatedBy: maya, Owner: owner}},
		{Kind: "doc", UUID: "b", Title: "Brief", Data: &dgraphStruct.DgraphDoc{Uuid: "b", Owner: owner}},
		{Kind: "doc", UUID: "c", Title: "New", Data: &dgraphStruct.DgraphDoc{Uuid: "c", UpdatedBy: &dgraphStruct.DgraphUser{Uid: "0x9"}}},
	}, nil))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	_ = json.Unmarshal(raw, &rows)
	by, _ := rows[0]["updated_by"].(map[string]any)
	if by["user_uuid"] != maya.Uuid || by["user_name"] != "Maya" || by["user_full_name"] != "Maya Chen" {
		t.Errorf("a tree row's last editor: %v", rows[0])
	}
	for _, r := range rows[1:] {
		if _, ok := r["updated_by"]; ok {
			t.Errorf("a row with no one named names someone: %v", r)
		}
	}

	old := accessBusiness.Rules
	accessBusiness.Rules = fixedRules{d: accessBusiness.Decision{Level: accessBusiness.LevelView}}
	t.Cleanup(func() { accessBusiness.Rules = old })
	items, err := listItems(context.Background(), accessBusiness.Subject{UserUID: "0x1", UserUUID: "u-1"}, []*dgraphStruct.DgraphDoc{
		{Uuid: "a", Title: "Plan", UpdatedBy: maya, Owner: owner},
		{Uuid: "b", Title: "Brief", Owner: owner},
	})
	if err != nil || len(items) != 2 {
		t.Fatalf("%+v %v", items, err)
	}
	raw, _ = json.Marshal(items)
	var list []map[string]any
	_ = json.Unmarshal(raw, &list)
	if by, _ := list[0]["updated_by"].(map[string]any); by["user_uuid"] != maya.Uuid {
		t.Errorf("a list row's last editor: %v", list[0])
	}
	if _, ok := list[1]["updated_by"]; ok || list[1]["owner"] == nil {
		t.Errorf("a list row with no editor names one, or lost its owner: %v", list[1])
	}
}

// Only a member's rows name the last editor (DOCS-LEAD-2's ruling): a guest's
// surfaces and a published page show no name they didn't show before. They
// read docs through reads of their own and never the member rows' (the
// tree's and the lists' reads, TreeNode, ListItem) or the editor; and a
// published page holds no person at all.
func TestGuestAndPublishedSurfacesNameNoEditor(t *testing.T) {
	members := []string{"docRowFields", "GetDocRowsByUUID", "GetDocWithChildren", "GetDocTreeTop", "treeNodes(", "listItems(", "TreeNode", "ListItem", "UpdatedBy", "doc_updated_by", "updated_by"}
	for _, dir := range []string{"../Guest", "../Publish", "../../business/Guest", "../../business/Publish"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no sources in %s: %v", dir, err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range members {
				if strings.Contains(string(src), m) {
					t.Errorf("%s reads a member's row or its editor: %s", f, m)
				}
			}
		}
	}
	person := reflect.TypeOf(dgraphStruct.DgraphUser{})
	page := reflect.TypeOf(publishBusiness.Page{})
	for i := 0; i < page.NumField(); i++ {
		f := page.Field(i)
		typ := f.Type
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
			typ = typ.Elem()
		}
		if typ == person || strings.Contains(strings.ToLower(f.Name), "by") || strings.Contains(strings.ToLower(f.Name), "editor") || strings.Contains(strings.ToLower(f.Name), "owner") {
			t.Errorf("a published page holds a person: %s %s", f.Name, f.Type)
		}
	}
}
