//go:build integration

package business

// Links to channels and calls (airtable-01): a relation field whose target is
// "channel" or "call" (or "any") names each link for its reader, by the
// channel's own access rule and by the call's (business/CallState: whoever may
// read its conversation's calls), from the one shared title resolver
// (business/Access ReadableTitles), and in a number of reads that doesn't
// grow with the links a page holds.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	_ "github.com/akashc777/OneCamp/business/CallState" // registers the call's title reader
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	_ "github.com/akashc777/OneCamp/business/EntityLink" // registers the channel's
	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// countingRules counts every decision the Access Resolver is asked for.
type countingRules struct {
	accessBusiness.Resolver
	n *atomic.Int32
}

func (c countingRules) Resolve(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource) (accessBusiness.Decision, error) {
	c.n.Add(1)
	return c.Resolver.Resolve(ctx, who, res)
}

func (c countingRules) ResolveAll(ctx context.Context, who accessBusiness.Subject, res []accessBusiness.Resource) (map[accessBusiness.Resource]accessBusiness.Decision, error) {
	c.n.Add(1)
	return c.Resolver.(accessBusiness.Batch).ResolveAll(ctx, who, res)
}

func TestCallAndChannelLinksAreNamedForTheirReader(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	// Maya is in #leadership (private), #general and #launch (public), and
	// #old, deleted; Bo is in #general alone.
	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, name := range []string{"maya", "bo"} {
		id := uuid.New()
		people[name] = id
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	zero := "0001-01-01T00:00:00Z"
	channels := map[string]string{}
	for _, c := range []struct {
		name    string
		private bool
		deleted bool
		members []string
	}{{"leadership", true, false, []string{"maya"}}, {"general", false, false, []string{"maya", "bo"}},
		{"launch", false, false, []string{"maya"}}, {"old", false, true, []string{"maya"}}} {
		id := uuid.New()
		channels[c.name] = id.String()
		if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, $2, $3)`, id, c.name, c.private); err != nil {
			t.Fatal(err)
		}
		gone := zero
		if c.deleted {
			gone = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		}
		var members []map[string]any
		for _, m := range c.members {
			members = append(members, map[string]any{"uid": "_:" + m})
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + c.name, "dgraph.type": "Channel", "ch_uuid": id.String(), "ch_name": c.name,
			"ch_private": c.private, "ch_deleted_at": gone, "ch_members": members})
	}
	dg.Mutate(t, nodes)

	// Calls: one in #leadership with a topic, one in #general without, and an
	// instant meeting's, which belongs to no conversation.
	started := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	call := func(kind, resource, topic string, at time.Time) string {
		t.Helper()
		end := at.Add(30 * time.Minute)
		room := resource
		if kind == "meeting" {
			room = "meet-" + resource
		}
		s := callStateModel.Session{ID: uuid.New(), Room: room, Kind: callStateModel.KindHuddle, ResourceKind: kind, ResourceID: resource, StartedAt: at, EndedAt: &end, EndedReason: callStateModel.EndedLastLeft}
		if err := callStateModel.KeepEndedSession(ctx, s, nil, nil); err != nil {
			t.Fatalf("keep a call in %s: %v", resource, err)
		}
		if topic != "" {
			if _, err := env.PG.Exec(`UPDATE call_sessions SET topic = $2 WHERE id = $1`, s.ID, topic); err != nil {
				t.Fatal(err)
			}
		}
		return s.ID.String()
	}
	reorg := call("channel", channels["leadership"], "Reorg sync", started)
	standup := call("channel", channels["general"], "", started)
	instant := call("meeting", uuid.NewString(), "Vendor call", started)

	maya, bo := Actor{UserID: people["maya"]}, Actor{UserID: people["bo"]}
	tb, err := CreateTable(ctx, TableInput{Name: "Launch log", Visibility: "workspace"}, maya)
	if err != nil {
		t.Fatal(err)
	}
	field := func(name, target string) *model.Field {
		t.Helper()
		f, err := CreateField(ctx, tb.Id, FieldInput{Name: name, Type: model.FieldRelation, Config: map[string]interface{}{"relation_target": target}}, maya)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if got := fieldTypes.RelationTarget(f); got != target {
			t.Fatalf("%s links to %q, want %q", name, got, target)
		}
		return f
	}
	chField, callField, anyField := field("Channels", "channel"), field("Calls", "call"), field("Anything", "any")
	ref := func(typ, id string) map[string]any { return map[string]any{"id": id, "type": typ} }
	row, err := CreateRow(ctx, tb.Id, RowInput{Values: map[string]interface{}{
		chField.Id.String():   []any{ref("channel", channels["leadership"]), ref("channel", channels["general"]), ref("channel", channels["launch"]), ref("channel", channels["old"])},
		callField.Id.String(): []any{ref("call", reorg), ref("call", standup), ref("call", instant)},
		anyField.Id.String():  []any{ref("channel", channels["general"]), ref("call", reorg)},
	}}, maya)
	if err != nil {
		t.Fatalf("link channels and calls: %v", err)
	}

	read := func(ctx context.Context, as *Actor) map[string][]fieldTypes.ItemRef {
		t.Helper()
		var b *TableBundle
		var err error
		if as == nil {
			b, err = GetGuestBundle(ctx, tb.Id)
		} else {
			b, err = GetBundle(ctx, tb.Id, *as)
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		out := map[string][]fieldTypes.ItemRef{}
		for _, r := range b.Rows {
			if r.Id != row.Id {
				continue
			}
			for id, v := range parseRowValues(r.Values) {
				raw, _ := json.Marshal(v)
				var refs []fieldTypes.ItemRef
				if json.Unmarshal(raw, &refs) == nil {
					out[id] = refs
				}
			}
		}
		return out
	}
	show := func(refs []fieldTypes.ItemRef) string {
		var s []string
		for _, r := range refs {
			if r.Hidden {
				s = append(s, r.Type+":hidden")
			} else {
				s = append(s, r.Type+":"+r.Label)
			}
		}
		return strings.Join(s, " | ")
	}
	day := started.Format("2 Jan 2006")

	t.Run("a member reads what they may open, and the rest hidden and unnamed", func(t *testing.T) {
		m, b := read(ctx, &maya), read(ctx, &bo)
		if got := show(m[chField.Id.String()]); got != "channel:leadership | channel:general | channel:launch | channel:hidden" {
			t.Errorf("Maya's channels: %s", got)
		}
		if got := show(b[chField.Id.String()]); got != "channel:hidden | channel:general | channel:launch | channel:hidden" {
			t.Errorf("Bo's channels (a public channel named to him, a private one hidden): %s", got)
		}
		if got, want := show(m[callField.Id.String()]), "call:Reorg sync | call:Call in #general, "+day+" | call:hidden"; got != want {
			t.Errorf("Maya's calls: %s, want %s", got, want)
		}
		if got, want := show(b[callField.Id.String()]), "call:hidden | call:Call in #general, "+day+" | call:hidden"; got != want {
			t.Errorf("Bo's calls: %s, want %s", got, want)
		}
		if got, want := show(b[anyField.Id.String()]), "channel:general | call:hidden"; got != want {
			t.Errorf("Bo's anything: %s, want %s", got, want)
		}
		for _, r := range b[callField.Id.String()] {
			if r.Hidden && (r.Label != "" || r.ID == "") {
				t.Errorf("a hidden call carries %+v", r)
			}
		}
	})

	t.Run("a guest's read leaves channels and calls out", func(t *testing.T) {
		g := read(ctx, nil)
		for _, f := range []*model.Field{chField, callField, anyField} {
			if len(g[f.Id.String()]) != 0 {
				t.Errorf("a guest reads %s: %+v", f.Name, g[f.Id.String()])
			}
		}
	})

	t.Run("a link of a kind the field doesn't take is refused", func(t *testing.T) {
		for f, wrong := range map[*model.Field]map[string]any{chField: ref("call", reorg), callField: ref("channel", channels["general"]), anyField: ref("meeting", uuid.NewString())} {
			if _, err := UpdateRow(ctx, tb.Id, row.Id, RowInput{Values: map[string]interface{}{f.Id.String(): []any{wrong}}}, maya); err == nil {
				t.Errorf("%s took %v", f.Name, wrong)
			}
		}
	})

	// The 7th check: a page of 60 calls across 3 conversations is named in
	// as many reads as a page of 3.
	t.Run("a page of calls is named in a constant number of reads", func(t *testing.T) {
		resolves := func(n int) int32 {
			t.Helper()
			tb2, err := CreateTable(ctx, TableInput{Name: fmt.Sprintf("Calls %d", n), Visibility: "workspace"}, maya)
			if err != nil {
				t.Fatal(err)
			}
			f, err := CreateField(ctx, tb2.Id, FieldInput{Name: "Call", Type: model.FieldRelation, Config: map[string]interface{}{"relation_target": "call"}}, maya)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < n; i++ {
				home := []string{channels["leadership"], channels["general"], channels["launch"]}[i%3]
				id := call("channel", home, fmt.Sprintf("Sync %d", i), started.Add(time.Duration(i)*time.Hour))
				if _, err := CreateRow(ctx, tb2.Id, RowInput{Values: map[string]interface{}{f.Id.String(): []any{ref("call", id)}}}, maya); err != nil {
					t.Fatal(err)
				}
			}
			var count atomic.Int32
			saved := accessBusiness.Rules
			accessBusiness.Rules = countingRules{Resolver: saved, n: &count}
			defer func() { accessBusiness.Rules = saved }()
			b, err := GetBundle(ctx, tb2.Id, maya)
			if err != nil {
				t.Fatal(err)
			}
			named := 0
			for _, r := range b.Rows {
				raw, _ := json.Marshal(parseRowValues(r.Values)[f.Id.String()])
				var refs []fieldTypes.ItemRef
				_ = json.Unmarshal(raw, &refs)
				if len(refs) == 1 && strings.HasPrefix(refs[0].Label, "Sync ") {
					named++
				}
			}
			if named != n {
				t.Errorf("%d of %d calls named for Maya", named, n)
			}
			return count.Load()
		}
		few, many := resolves(3), resolves(60)
		if few == 0 || many != few {
			t.Errorf("Access decisions for 3 calls: %d, for 60: %d; want the same, above 0", few, many)
		}
	})
}

// A member removed from a channel through the real path (DeleteChannelMemberEdge,
// which clears the 30-minute ChannelBasicInfo cache the call rule reads
// standing through) reads the channel's link and its call hidden and unnamed
// at once; a removal path that skipped that clearing would leave both named
// (CR-374 NB1: cutting the graph edge alone still named the call).
func TestARemovedMemberLosesTheCallAndChannelNames(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	maya, bo := uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"maya": maya, "bo": bo} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name); err != nil {
			t.Fatal(err)
		}
	}
	ch := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'secret', true)`, ch); err != nil {
		t.Fatal(err)
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": maya.String(), "user_name": "maya"},
		{"uid": "_:bo", "dgraph.type": "User", "user_uuid": bo.String(), "user_name": "bo"},
		{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": ch.String(), "ch_name": "secret", "ch_private": true,
			"ch_deleted_at": "0001-01-01T00:00:00Z", "ch_members": []map[string]any{{"uid": "_:maya"}, {"uid": "_:bo"}}},
	})
	at := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	end := at.Add(30 * time.Minute)
	s := callStateModel.Session{ID: uuid.New(), Room: ch.String(), Kind: callStateModel.KindHuddle, ResourceKind: "channel", ResourceID: ch.String(), StartedAt: at, EndedAt: &end, EndedReason: callStateModel.EndedLastLeft}
	if err := callStateModel.KeepEndedSession(ctx, s, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`UPDATE call_sessions SET topic = 'Layoffs' WHERE id = $1`, s.ID); err != nil {
		t.Fatal(err)
	}
	m, b := Actor{UserID: maya}, Actor{UserID: bo}
	tb, err := CreateTable(ctx, TableInput{Name: "Probe", Visibility: "workspace"}, m)
	if err != nil {
		t.Fatal(err)
	}
	f, err := CreateField(ctx, tb.Id, FieldInput{Name: "Where", Type: model.FieldRelation, Config: map[string]interface{}{"relation_target": "any"}}, m)
	if err != nil {
		t.Fatal(err)
	}
	row, err := CreateRow(ctx, tb.Id, RowInput{Values: map[string]interface{}{
		f.Id.String(): []any{map[string]any{"id": ch.String(), "type": "channel"}, map[string]any{"id": s.ID.String(), "type": "call"}},
	}}, m)
	if err != nil {
		t.Fatal(err)
	}
	read := func() string {
		t.Helper()
		bundle, err := GetBundle(ctx, tb.Id, b)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range bundle.Rows {
			if r.Id != row.Id {
				continue
			}
			raw, _ := json.Marshal(parseRowValues(r.Values)[f.Id.String()])
			var refs []fieldTypes.ItemRef
			_ = json.Unmarshal(raw, &refs)
			var out []string
			for _, ref := range refs {
				if ref.Hidden {
					out = append(out, ref.Type+":hidden")
				} else {
					out = append(out, ref.Type+":"+ref.Label)
				}
			}
			return strings.Join(out, " | ")
		}
		return ""
	}
	if got := read(); got != "channel:secret | call:Layoffs" {
		t.Fatalf("Bo as a member reads %q", got)
	}
	if err := channelBusiness.DeleteChannelMemberEdge(ctx, uids["ch"], uids["bo"], bo.String(), ch.String()); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "channel:hidden | call:hidden" {
		t.Errorf("Bo removed reads %q, want both hidden", got)
	}
}
