package business

import (
	"context"
	"reflect"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// HiddenUnassignedAmong is HiddenUnassigned for many readers, from three
// reads made once for them all: the same answer for each reader, whatever
// lets them in (granted, running the project, a role row saying less than
// the graph's admin edge, the task above), and a page of people costs three
// reads, not three each.
func TestHiddenUnassignedAmongDecidesAsHiddenUnassignedDoes(t *testing.T) {
	me := uuid.MustParse("5a5c00a8-0000-4000-8000-0000000000aa")  // a member of A who runs B
	ann := uuid.MustParse("5a5c00a8-0000-4000-8000-0000000000ab") // A's admin in the graph, a member by her role row; granted on kept
	zed := uuid.MustParse("5a5c00a8-0000-4000-8000-0000000000ac") // runs A
	mine := uuid.MustParse("7a5c00a8-0000-4000-8000-000000000001")
	run := uuid.MustParse("7a5c00a8-0000-4000-8000-000000000002")
	kept := uuid.MustParse("7a5c00a8-0000-4000-8000-000000000003")
	underMine := uuid.MustParse("7a5c00a8-0000-4000-8000-000000000005")
	underKept := uuid.MustParse("7a5c00a8-0000-4000-8000-000000000006")
	granted := uuid.MustParse("7a5c00a8-0000-4000-8000-000000000008")
	projectA, projectB := uuid.New(), uuid.New()
	type stands struct{ member, admin map[uuid.UUID]bool }
	place := map[string]stands{
		me.String():  {member: map[uuid.UUID]bool{projectA: true, projectB: true}, admin: map[uuid.UUID]bool{projectB: true}},
		ann.String(): {member: map[uuid.UUID]bool{projectA: true}, admin: map[uuid.UUID]bool{projectA: true}},
		zed.String(): {member: map[uuid.UUID]bool{projectA: true}, admin: map[uuid.UUID]bool{projectA: true}},
	}
	rowsOf := func(reader uuid.UUID) []restrictionModel.Candidate {
		return []restrictionModel.Candidate{
			{TaskID: mine, ProjectID: projectA}, {TaskID: run, ProjectID: projectB},
			{TaskID: kept, ProjectID: projectA, LetIn: reader == ann},
			{TaskID: underMine, ProjectID: projectA, Via: mine}, {TaskID: underKept, ProjectID: projectA, Via: kept},
			{TaskID: granted, ProjectID: projectA, LetIn: reader == me},
		}
	}
	projectOf := map[uuid.UUID]uuid.UUID{mine: projectA, run: projectB, kept: projectA, underMine: projectA, underKept: projectA, granted: projectA}
	standing := func(ids []string, user string) []*dgraphStruct.DgraphTask {
		var out []*dgraphStruct.DgraphTask
		for _, id := range ids {
			task := uuid.MustParse(id)
			p := projectOf[task]
			bit := func(on bool) uint8 {
				if on {
					return 1
				}
				return 0
			}
			t := &dgraphStruct.DgraphTask{Uuid: id, Project: &dgraphStruct.DgraphProject{Uuid: p.String(),
				IsProjectMember: bit(place[user].member[p]), IsProjectAdmin: bit(place[user].admin[p])}}
			if task == mine {
				t.Assignee = &dgraphStruct.DgraphUser{Uuid: me.String()}
			}
			out = append(out, t)
		}
		return out
	}
	rolesOf := func(user uuid.UUID) map[uuid.UUID]string {
		if user == ann {
			return map[uuid.UUID]string{projectA: "member"}
		}
		return nil
	}
	oldC, oldR, oldRoles, oldWho, oldTok := restrictionCandidates, readStanding, rolesIn, editWho, viaToken
	oldCA, oldSA, oldRA := candidatesAmong, standingAmong, rolesAmong
	t.Cleanup(func() {
		restrictionCandidates, readStanding, rolesIn, editWho, viaToken = oldC, oldR, oldRoles, oldWho, oldTok
		candidatesAmong, standingAmong, rolesAmong = oldCA, oldSA, oldRA
	})
	restrictionCandidates = func(_ context.Context, reader uuid.UUID) ([]restrictionModel.Candidate, error) {
		return rowsOf(reader), nil
	}
	readStanding = func(_ context.Context, ids []string, user string) ([]*dgraphStruct.DgraphTask, error) {
		return standing(ids, user), nil
	}
	rolesIn = func(_ context.Context, user uuid.UUID, _ []uuid.UUID) (map[uuid.UUID]string, error) {
		return rolesOf(user), nil
	}
	editWho = func(context.Context, string) string { return EditMembers }
	viaToken = func(context.Context) bool { return false }
	reads := map[string]int{}
	candidatesAmong = func(_ context.Context, readers []uuid.UUID) (map[uuid.UUID][]restrictionModel.Candidate, error) {
		reads["rows"]++
		out := map[uuid.UUID][]restrictionModel.Candidate{}
		for _, r := range readers {
			out[r] = rowsOf(r)
		}
		return out, nil
	}
	standingAmong = func(ctx context.Context, ids, users []string) (map[string][]*dgraphStruct.DgraphTask, error) {
		reads["graph"]++
		if !taskread.System(ctx) {
			t.Error("the graph is read for the decision as a read with no audience")
		}
		out := map[string][]*dgraphStruct.DgraphTask{}
		for _, u := range users {
			out[u] = standing(ids, u)
		}
		return out, nil
	}
	rolesAmong = func(_ context.Context, users, _ []uuid.UUID) (map[uuid.UUID]map[uuid.UUID]string, error) {
		reads["roles"]++
		out := map[uuid.UUID]map[uuid.UUID]string{}
		for _, u := range users {
			out[u] = rolesOf(u)
		}
		return out, nil
	}

	// Once as the people themselves, and once through an API token, which
	// acts as a member wherever its person runs the project.
	for _, token := range []bool{false, true} {
		viaToken = func(context.Context) bool { return token }
		clear(reads)
		got, err := HiddenUnassignedAmong(context.Background(), []string{me.String(), ann.String(), zed.String()})
		if err != nil {
			t.Fatal(err)
		}
		if reads["rows"] != 1 || reads["graph"] != 1 || reads["roles"] != 1 {
			t.Errorf("token %v: reads %v, want each once for the three readers", token, reads)
		}
		for _, r := range []uuid.UUID{me, ann, zed} {
			want, err := hidden(context.Background(), r.String(), false)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got[r.String()], want) {
				t.Errorf("token %v: %s: among %v, alone %v", token, r, got[r.String()], want)
			}
		}
		// What each is let into, so the comparison above is about something.
		if token {
			// Running a project lets no token in: me is kept out of B's run,
			// and zed out of A's, while a grant still lets me in.
			if !got[me.String()][run.String()] || !got[zed.String()][mine.String()] || got[me.String()][granted.String()] {
				t.Errorf("through a token, decided %v", got)
			}
			continue
		}
		if !got[me.String()][mine.String()] || got[me.String()][run.String()] || got[ann.String()][kept.String()] ||
			!got[ann.String()][mine.String()] || got[zed.String()][mine.String()] || got[zed.String()][underKept.String()] {
			t.Errorf("decided %v", got)
		}
	}
}
