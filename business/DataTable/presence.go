package business

// Presence on a table (tables A7, Live Rows #65; WIN-ALL-ROWS notion-db-10):
// who has it open, and the cell each has selected or is editing, so the grid
// shows their faces in its toolbar, a border in their colour on their cell,
// and "Maya is editing" on a cell being edited. Kept in Redis, each person to
// their place, and told on the table's live topic when someone comes, moves
// or goes, never for a heartbeat alone. An open table says it is there every
// 20 seconds; someone not heard from in presenceFor has gone.

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	"github.com/akashc777/OneCamp/helpers"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/models/redis/registry"
	"github.com/google/uuid"
)

// Here is one person on a table: where they are, and when they were last
// heard from (unix ms).
type Here struct {
	User string `json:"user"`
	// Name is who they are, as a table names people; read as it's answered, never kept.
	Name    string `json:"name,omitempty"`
	Row     string `json:"row,omitempty"`
	Field   string `json:"field,omitempty"`
	Editing bool   `json:"editing,omitempty"`
	At      int64  `json:"at"`
}

// presenceFor is how long someone not heard from is still there.
const presenceFor = 45 * time.Second

// presenceStore keeps each table's people. A seam for tests.
type presenceStore interface {
	put(ctx context.Context, table string, h Here) error
	drop(ctx context.Context, table string, users ...string) error
	all(ctx context.Context, table string) ([]Here, error)
}

var presence presenceStore = redisPresence{}

// tellPresence says who is on a table now, on its live topic. A seam for tests.
var tellPresence = func(table string, people []Here) {
	mqttBusiness.PublishTablePresence(table, map[string]interface{}{"action": "presence", "people": people})
}

// SetPresence records where actor is on a table (gone: that they left) and
// answers everyone there now, actor too unless they left. A reader who can't
// open the table is refused. Row and field are ids or nothing; anything else
// is nothing. The live topic hears of it only when someone came, moved or went.
func SetPresence(ctx context.Context, table uuid.UUID, actor Actor, at Here, gone bool, now time.Time) ([]Here, error) {
	if _, err := loadViewable(ctx, table, actor); err != nil {
		return nil, err
	}
	me := Here{User: actor.UserID.String(), Row: idOrNothing(at.Row), Field: idOrNothing(at.Field), Editing: at.Editing, At: now.UnixMilli()}
	if me.Row == "" || me.Field == "" {
		me.Editing = false
	}
	key := table.String()
	before, err := presence.all(ctx, key)
	if err != nil {
		return nil, err
	}
	people, stale := livePresence(before, now)
	var was *Here
	for i := range people {
		if people[i].User == me.User {
			was = &people[i]
		}
	}
	drop := stale
	if gone {
		drop = append(drop, me.User)
	} else if err := presence.put(ctx, key, me); err != nil {
		return nil, err
	}
	if len(drop) > 0 {
		if err := presence.drop(ctx, key, drop...); err != nil {
			helpers.LogErrorWithContext(ctx, "business/DataTable SetPresence: drop: %v", err)
		}
	}
	after := named(ctx, withPresence(people, me, gone))
	// Others hear when I came, moved or went, or when someone went silent.
	if presenceMoved(was, me, gone) || len(stale) > 0 {
		tellPresence(key, after)
	}
	return after, nil
}

// named is people with their names, as a table names people (the names that
// can't be read leave them unnamed). A seam for tests.
var named = func(ctx context.Context, people []Here) []Here {
	ids := make([]uuid.UUID, 0, len(people))
	for _, h := range people {
		if id, err := uuid.Parse(h.User); err == nil {
			ids = append(ids, id)
		}
	}
	names, err := model.PeopleNames(ctx, ids)
	if err != nil {
		return people
	}
	for i := range people {
		if id, err := uuid.Parse(people[i].User); err == nil {
			people[i].Name = names[id]
		}
	}
	return people
}

// livePresence is the people heard from within presenceFor, by when they
// came (oldest first, then by id), and the ids of those gone. Pure.
func livePresence(all []Here, now time.Time) (live []Here, gone []string) {
	cut := now.Add(-presenceFor).UnixMilli()
	for _, h := range all {
		if h.At < cut {
			gone = append(gone, h.User)
			continue
		}
		live = append(live, h)
	}
	sort.SliceStable(live, func(i, j int) bool { return live[i].User < live[j].User })
	return live, gone
}

// withPresence is people with me put in (or taken out, gone). Pure.
func withPresence(people []Here, me Here, gone bool) []Here {
	out := make([]Here, 0, len(people)+1)
	for _, h := range people {
		if h.User != me.User {
			out = append(out, h)
		}
	}
	if !gone {
		out = append(out, me)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].User < out[j].User })
	return out
}

// presenceMoved is whether others should hear of me: I came, went, or am
// at another cell, or started or stopped editing. A heartbeat is none. Pure.
func presenceMoved(was *Here, me Here, gone bool) bool {
	if gone {
		return was != nil
	}
	return was == nil || was.Row != me.Row || was.Field != me.Field || was.Editing != me.Editing
}

// idOrNothing is s when it's an id, else "".
func idOrNothing(s string) string {
	id, err := uuid.Parse(s)
	if err != nil {
		return ""
	}
	return id.String()
}

// redisPresence keeps presence in Redis (registry.TablePresence).
type redisPresence struct{}

func (redisPresence) put(ctx context.Context, table string, h Here) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	key := registry.TablePresence.Build(table)
	pipe := redisInit.RedisClient.TxPipeline()
	pipe.HSet(ctx, key, h.User, b)
	pipe.Expire(ctx, key, registry.TablePresence.TTL)
	_, err = pipe.Exec(ctx)
	return err
}

func (redisPresence) drop(ctx context.Context, table string, users ...string) error {
	return redisInit.RedisClient.HDel(ctx, registry.TablePresence.Build(table), users...).Err()
}

func (redisPresence) all(ctx context.Context, table string) ([]Here, error) {
	got, err := redisInit.RedisClient.HGetAll(ctx, registry.TablePresence.Build(table)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]Here, 0, len(got))
	for _, v := range got {
		var h Here
		if json.Unmarshal([]byte(v), &h) == nil && h.User != "" {
			out = append(out, h)
		}
	}
	return out, nil
}
