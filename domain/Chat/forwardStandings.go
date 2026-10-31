package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Standings is where a forward's groups and people stand, read at once: each
// group by its grouping id (its node, and whether the sender is in it), each
// person by their uuid (what the rule for writing to them reads,
// business/Send MayMessage). An id the graph doesn't have is left out.
type Standings struct {
	Groups map[string]*dgraphStruct.DgraphDm
	People map[string]*dgraphStruct.DgraphUser
}

// standingsQuery is the one read for a forward's groups and people: a block
// per distinct id, each id a query variable, so nothing from the request
// reaches the query text. A forward to 100 places used to read each group
// and each person on its own (DG4's probe: N+2 trips). Pure.
func standingsQuery(senderUID string, grpIDs, userUUIDs []string) (string, map[string]string) {
	vars := map[string]string{"$me": senderUID}
	decls := []string{"$me: string"}
	var blocks []string
	for i, id := range distinct(grpIDs) {
		name := fmt.Sprintf("g%d", i)
		vars["$"+name] = id
		decls = append(decls, "$"+name+": string")
		blocks = append(blocks, name+`(func: eq(dm_grouping_id, $`+name+`)) {
			uid
			dm_grouping_id
			dm_is_member: count(dm_participants @filter(uid($me)))
		}`)
	}
	for i, id := range distinct(userUUIDs) {
		name := fmt.Sprintf("p%d", i)
		vars["$"+name] = id
		decls = append(decls, "$"+name+": string")
		blocks = append(blocks, name+`(func: eq(user_uuid, $`+name+`)) {
			uid
			user_uuid
			user_name
			user_profile_object_key
			user_deleted_at
			is_external
			is_bot
		}`)
	}
	return "query standings(" + strings.Join(decls, ", ") + ") {\n" + strings.Join(blocks, "\n") + "\n}", vars
}

// distinct is ids each once, in their first order, without empty ones.
func distinct(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// readStandings files a read's answer by id. Pure.
func readStandings(answer []byte) (Standings, error) {
	var blocks map[string]json.RawMessage
	if err := json.Unmarshal(answer, &blocks); err != nil {
		return Standings{}, err
	}
	out := Standings{Groups: map[string]*dgraphStruct.DgraphDm{}, People: map[string]*dgraphStruct.DgraphUser{}}
	for name, raw := range blocks {
		switch {
		case strings.HasPrefix(name, "g"):
			var dms []*dgraphStruct.DgraphDm
			if err := json.Unmarshal(raw, &dms); err != nil {
				return Standings{}, err
			}
			for _, dm := range dms {
				out.Groups[dm.GroupingId] = dm
			}
		case strings.HasPrefix(name, "p"):
			var users []*dgraphStruct.DgraphUser
			if err := json.Unmarshal(raw, &users); err != nil {
				return Standings{}, err
			}
			for _, u := range users {
				out.People[u.Uuid] = u
			}
		}
	}
	return out, nil
}

// StandingsOf reads where a forward's groups and people stand, in one trip.
func StandingsOf(ctx context.Context, senderUID string, grpIDs, userUUIDs []string) (Standings, error) {
	if len(distinct(grpIDs)) == 0 && len(distinct(userUUIDs)) == 0 {
		return Standings{Groups: map[string]*dgraphStruct.DgraphDm{}, People: map[string]*dgraphStruct.DgraphUser{}}, nil
	}
	if dgraphInit.DgraphClient == nil {
		return Standings{}, fmt.Errorf("no graph connection")
	}
	query, vars := standingsQuery(senderUID, grpIDs, userUUIDs)
	// Through models/dgexec, as every read is (helpers executorGuard).
	answer, err := dgexec.Read[json.RawMessage](ctx, query, vars)
	if err != nil {
		return Standings{}, err
	}
	if len(answer) == 0 {
		return Standings{Groups: map[string]*dgraphStruct.DgraphDm{}, People: map[string]*dgraphStruct.DgraphUser{}}, nil
	}
	return readStandings(answer)
}
