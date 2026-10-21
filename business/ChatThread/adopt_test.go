package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	threadIndex "github.com/akashc777/OneCamp/business/ThreadIndex"
	domain "github.com/akashc777/OneCamp/domain/Message"
)

// sweepWorld is the graph's roots of each kind, oldest first, and what the
// sweep keeps between passes.
type sweepWorld struct {
	posts, chats []domain.ThreadRoot
	saved        string
	adopted      []threadIndex.Thread
	rows         map[string]bool
	asked        []time.Time
	failRoots    error
}

func stubSweep(t *testing.T) *sweepWorld {
	t.Helper()
	w := &sweepWorld{rows: map[string]bool{}}
	oldRoots, oldAdopt, oldDone, oldLoad, oldSave, oldNow := rootsSince, adoptAsRead, adoptDone, loadAdopt, saveAdopt, now
	t.Cleanup(func() {
		rootsSince, adoptAsRead, adoptDone, loadAdopt, saveAdopt, now = oldRoots, oldAdopt, oldDone, oldLoad, oldSave, oldNow
	})
	now = func() time.Time { return today }
	rootsSince = func(_ context.Context, channel bool, since time.Time, first int) ([]domain.ThreadRoot, error) {
		if w.failRoots != nil {
			return nil, w.failRoots
		}
		w.asked = append(w.asked, since)
		all := w.chats
		if channel {
			all = w.posts
		}
		var out []domain.ThreadRoot
		for _, r := range all {
			if !r.CreatedAt.Before(since) && len(out) < first {
				out = append(out, r)
			}
		}
		return out, nil
	}
	adoptAsRead = func(_ context.Context, th threadIndex.Thread, last time.Time, repliers []string) (bool, error) {
		if w.rows[th.Root] {
			return false, nil
		}
		w.rows[th.Root] = true
		w.adopted = append(w.adopted, th)
		return true, nil
	}
	adoptDone = func() (bool, error) { return strings.HasPrefix(w.saved, "done"), nil }
	loadAdopt = func() (string, error) { return w.saved, nil }
	saveAdopt = func(v string) error { w.saved = v; return nil }
	return w
}

func roots(n int, from time.Time, in string) []domain.ThreadRoot {
	out := make([]domain.ThreadRoot, n)
	for i := range out {
		at := from.Add(time.Duration(i) * time.Minute)
		out[i] = domain.ThreadRoot{ID: fmt.Sprintf("%s-%04d", in, i), In: in, By: maya, CreatedAt: at, Repliers: []string{jonas}, LastReply: at.Add(time.Hour)}
	}
	return out
}

// The sweep reads the last 90 days a few hundred roots a pass, goes on where
// it stopped, and records itself done once it has read up to now.
func TestTheAdoptionSweepGoesOnUntilDone(t *testing.T) {
	w := stubSweep(t)
	w.posts = roots(domain.MaxRead*10+7, today.Add(-80*24*time.Hour), "launch")
	w.chats = roots(3, today.Add(-time.Hour), maya+" "+jonas)
	r1, err := AdoptEarlier(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Each page after the first begins at the last one's last root, read
	// again and left as it is.
	pages := adoptPerPass / domain.MaxRead
	if r1.Done || r1.Read != adoptPerPass+3 || r1.Adopted != adoptPerPass-(pages-1)+3 {
		t.Errorf("first pass %+v: want %d posts read in %d pages and the 3 chats, not done", r1, adoptPerPass, pages)
	}
	if w.asked[0] != today.Add(-adoptWindow) {
		t.Errorf("began at %v, want the window's edge", w.asked[0])
	}
	var st adoptState
	if err := json.Unmarshal([]byte(w.saved), &st); err != nil || st.PostsDone || !st.ChatsDone {
		t.Fatalf("kept %q: want posts under way, chats done", w.saved)
	}
	passes := 1
	for !strings.HasPrefix(w.saved, "done") && passes < 30 {
		if _, err := AdoptEarlier(context.Background()); err != nil {
			t.Fatal(err)
		}
		passes++
	}
	if len(w.rows) != len(w.posts)+len(w.chats) {
		t.Errorf("adopted %d of %d threads in %d passes", len(w.rows), len(w.posts)+len(w.chats), passes)
	}
	if w.adopted[len(w.adopted)-1].Kind != threadIndex.KindChannel || w.adopted[0].Kind != threadIndex.KindChannel {
		t.Errorf("posts adopted as %q", w.adopted[0].Kind)
	}
	for _, th := range w.adopted {
		if th.ID == maya+" "+jonas && th.Kind != threadIndex.KindDM {
			t.Errorf("a DM's thread adopted as %q", th.Kind)
		}
	}
	asked := len(w.asked)
	if r, err := AdoptEarlier(context.Background()); err != nil || !r.Done || len(w.asked) != asked {
		t.Errorf("after done: %+v, err %v, read %d more", r, err, len(w.asked)-asked)
	}
}

// A page of roots all written in one instant moves on past it rather than
// reading it for ever.
func TestTheSweepMovesPastAPageOfOneInstant(t *testing.T) {
	w := stubSweep(t)
	same := today.Add(-time.Hour)
	for i := 0; i < domain.MaxRead; i++ {
		w.posts = append(w.posts, domain.ThreadRoot{ID: fmt.Sprintf("p%d", i), In: "launch", CreatedAt: same, Repliers: []string{jonas}})
	}
	w.posts = append(w.posts, domain.ThreadRoot{ID: "later", In: "launch", CreatedAt: same.Add(time.Second), Repliers: []string{jonas}})
	if _, err := AdoptEarlier(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !w.rows["later"] {
		t.Errorf("never reached the root after the instant (asked %v)", w.asked)
	}
}

// A failed read keeps where the sweep was, and says so.
func TestAFailedSweepKeepsItsPlace(t *testing.T) {
	w := stubSweep(t)
	w.failRoots = errors.New("graph down")
	if _, err := AdoptEarlier(context.Background()); err == nil {
		t.Error("a failed pass said nothing")
	}
	if strings.HasPrefix(w.saved, "done") {
		t.Errorf("recorded %q after failing", w.saved)
	}
}
