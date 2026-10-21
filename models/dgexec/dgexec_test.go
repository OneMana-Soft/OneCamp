package dgexec

import "testing"

func TestRequestCarriesEveryMutation(t *testing.T) {
	req, err := request(`{ q(func: eq(task_uuid, $id)) { task as uid } }`, map[string]string{"$id": "t1"},
		Mutation{Set: map[string]any{"uid": Var("task"), "task_name": "x"}, Cond: "@if(eq(len(task), 1))"},
		Mutation{DelNQuads: `uid(task) <task_label> * .`})
	if err != nil {
		t.Fatal(err)
	}
	if !req.CommitNow || len(req.Mutations) != 2 || req.Vars["$id"] != "t1" {
		t.Fatalf("request: %+v", req)
	}
	if got := string(req.Mutations[0].SetJson); got != `{"task_name":"x","uid":"uid(task)"}` {
		t.Fatalf("set json: %s", got)
	}
	if req.Mutations[0].Cond != "@if(eq(len(task), 1))" || string(req.Mutations[1].DelNquads) != `uid(task) <task_label> * .` {
		t.Fatalf("cond or nquads lost: %+v", req.Mutations)
	}
	if _, err := request("", nil, Mutation{Set: func() {}}); err == nil {
		t.Fatal("a value JSON can't hold is an error, not an empty mutation")
	}
}
