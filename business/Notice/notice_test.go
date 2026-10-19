package business

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

// world is what the members-edit notice decides by, for one test.
type world struct {
	editWho     string
	since       time.Time
	noSince     bool
	admins      map[string][]uuid.UUID // by graph uid
	madeBefore  map[uuid.UUID]string   // project → name, made before since
	anyOld      bool
	seen        map[string]bool
	adminOfErr  error
	marked      []string
	askedAdmins int
}

func (w *world) install(t *testing.T) {
	t.Helper()
	e, s, a, f, old, so, m := editWho, since, adminOf, firstMadeBefore, anyMadeBefore, seenOf, markSeen
	t.Cleanup(func() {
		editWho, since, adminOf, firstMadeBefore, anyMadeBefore, seenOf, markSeen = e, s, a, f, old, so, m
	})
	editWho = func(context.Context) string { return w.editWho }
	since = func(context.Context) (time.Time, bool, error) { return w.since, !w.noSince, nil }
	adminOf = func(_ context.Context, uid string) ([]uuid.UUID, error) {
		w.askedAdmins++
		return w.admins[uid], w.adminOfErr
	}
	firstMadeBefore = func(_ context.Context, ids []uuid.UUID, at time.Time) (uuid.UUID, string, bool, error) {
		if !at.Equal(w.since) {
			t.Errorf("asked about projects made before %v, want %v", at, w.since)
		}
		for _, id := range ids {
			if name, ok := w.madeBefore[id]; ok {
				return id, name, true, nil
			}
		}
		return uuid.Nil, "", false, nil
	}
	anyMadeBefore = func(context.Context, time.Time) (bool, error) { return w.anyOld, nil }
	seenOf = func(_ context.Context, _ uuid.UUID, keys []string) (map[string]bool, error) {
		out := map[string]bool{}
		for _, k := range keys {
			out[k] = w.seen[k]
		}
		return out, nil
	}
	markSeen = func(_ context.Context, _ uuid.UUID, key string) error {
		w.marked = append(w.marked, key)
		return nil
	}
}

func TestTheMembersEditNoticeIsForTheAdminsOfOlderProjects(t *testing.T) {
	old, fresh := uuid.New(), uuid.New()
	base := func() *world {
		return &world{
			editWho:    "members",
			since:      time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC),
			admins:     map[string][]uuid.UUID{"0xana": {fresh, old}, "0xnew": {fresh}},
			madeBefore: map[uuid.UUID]string{old: "Website"},
			anyOld:     true,
		}
	}
	shown := func(project, name string) []Shown {
		s := Shown{Key: MembersEdit}
		if project != "" {
			s.Data = map[string]string{"project_uuid": project, "project_name": name}
		}
		return []Shown{s}
	}
	for _, c := range []struct {
		name string
		edit func(*world)
		who  Person
		want []Shown
	}{
		{"the admin of a project made before", nil, Person{UserUID: "0xana"}, shown(old.String(), "Website")},
		{"the admin of projects made since only", nil, Person{UserUID: "0xnew"}, []Shown{}},
		{"someone who runs no project", nil, Person{UserUID: "0xben"}, []Shown{}},
		{"a workspace admin who runs no project", nil, Person{UserUID: "0xwes", WorkspaceAdmin: true}, shown("", "")},
		{"a workspace admin who runs an older project is sent to it", nil, Person{UserUID: "0xana", WorkspaceAdmin: true}, shown(old.String(), "Website")},
		{"a workspace admin of an install with no older projects", func(w *world) { w.anyOld = false }, Person{UserUID: "0xwes", WorkspaceAdmin: true}, []Shown{}},
		{"a workspace that keeps tasks with its admins", func(w *world) { w.editWho = "admins" }, Person{UserUID: "0xana", WorkspaceAdmin: true}, []Shown{}},
		{"members on the tasks assigned to them is still news", func(w *world) { w.editWho = "assigned" }, Person{UserUID: "0xana"}, shown(old.String(), "Website")},
		{"an install that never had a start", func(w *world) { w.noSince = true }, Person{UserUID: "0xana", WorkspaceAdmin: true}, []Shown{}},
		{"dismissed", func(w *world) { w.seen = map[string]bool{MembersEdit: true} }, Person{UserUID: "0xana", WorkspaceAdmin: true}, []Shown{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := base()
			if c.edit != nil {
				c.edit(w)
			}
			w.install(t)
			got, err := For(context.Background(), c.who)
			if err != nil || !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %+v %v, want %+v", got, err, c.want)
			}
		})
	}

	t.Run("a dismissed notice isn't decided again", func(t *testing.T) {
		w := base()
		w.seen = map[string]bool{MembersEdit: true}
		w.install(t)
		if _, err := For(context.Background(), Person{UserUID: "0xana"}); err != nil || w.askedAdmins != 0 {
			t.Errorf("asked %d times, %v", w.askedAdmins, err)
		}
	})

	t.Run("a notice that can't decide is an error, never shown on a guess", func(t *testing.T) {
		w := base()
		w.adminOfErr = errors.New("graph down")
		w.install(t)
		if got, err := For(context.Background(), Person{UserUID: "0xana", WorkspaceAdmin: true}); err == nil || got != nil {
			t.Errorf("got %+v %v", got, err)
		}
	})
}

func TestDismissingANotice(t *testing.T) {
	w := &world{}
	w.install(t)
	if err := Dismiss(context.Background(), uuid.New(), MembersEdit); err != nil {
		t.Fatal(err)
	}
	if err := Dismiss(context.Background(), uuid.New(), "no.such.notice"); !errors.Is(err, ErrUnknown) {
		t.Errorf("an unknown notice: %v", err)
	}
	if !reflect.DeepEqual(w.marked, []string{MembersEdit}) {
		t.Errorf("marked %v", w.marked)
	}
}
