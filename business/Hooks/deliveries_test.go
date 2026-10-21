package business

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	models "github.com/akashc777/OneCamp/models/postgres/WebhookDelivery"
	"github.com/google/uuid"
)

// An outgoing webhook is sent its events until they arrive: written down
// before anything is sent, tried again after a failure, given up on after
// MaxTries, with the same body and event id on every try; and who is sent
// what is asked again as each try goes.

// memStore is the deliveries and the webhooks, in memory.
type memStore struct {
	mu       sync.Mutex
	next     int64
	rows     map[int64]*models.Row
	webhooks map[uuid.UUID]*models.Target
	failing  map[uuid.UUID]int
	paused   map[uuid.UUID]string
}

func newMem() *memStore {
	return &memStore{rows: map[int64]*models.Row{}, webhooks: map[uuid.UUID]*models.Target{}, failing: map[uuid.UUID]int{}, paused: map[uuid.UUID]string{}}
}

func (m *memStore) insert(_ context.Context, news []models.New) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []int64
	for _, n := range news {
		dup := false
		for _, r := range m.rows {
			if r.WebhookID == n.WebhookID && r.EventID == n.EventID {
				dup = true
			}
		}
		if dup {
			continue
		}
		m.next++
		m.rows[m.next] = &models.Row{ID: m.next, WebhookID: n.WebhookID, EventID: n.EventID, EventType: n.EventType, Payload: n.Payload, State: models.StatePending}
		ids = append(ids, m.next)
	}
	return ids, nil
}

// untried counts the rows still pending with no try made, read under the
// store's lock (sends write it from their own goroutines).
func (m *memStore) untried() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.rows {
		if r.State == models.StatePending && r.Attempts == 0 {
			n++
		}
	}
	return n
}

func (m *memStore) claimDue(context.Context, string, time.Duration, int) ([]models.Row, error) {
	return nil, nil
}
func (m *memStore) claimOne(_ context.Context, id int64, _ string, _ time.Duration) (models.Row, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok || r.State != models.StatePending {
		return models.Row{}, false, nil
	}
	r.Attempts++
	return *r, true, nil
}
func (m *memStore) finish(_ context.Context, id int64, _ string, state string, o models.Outcome) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rows[id]
	r.State, r.LastStatus = state, o.Status
	if o.Error != "" {
		r.LastError = &o.Error
	}
	return true, nil
}
func (m *memStore) retry(_ context.Context, id int64, _ string, at time.Time, o models.Outcome) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rows[id]
	r.NextAt, r.LastStatus = at, o.Status
	if o.Error != "" {
		r.LastError = &o.Error
	}
	return true, nil
}
func (m *memStore) target(_ context.Context, id uuid.UUID) (models.Target, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.webhooks[id]
	if !ok {
		return models.Target{}, false, nil
	}
	c := *t
	if why, paused := m.paused[id]; paused && why != "" {
		at := time.Unix(1, 0)
		c.PausedAt = &at
	}
	return c, true, nil
}
func (m *memStore) tried(_ context.Context, id uuid.UUID, ok bool) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ok {
		m.failing[id] = 0
	} else {
		m.failing[id]++
	}
	return m.failing[id], nil
}
func (m *memStore) setPaused(_ context.Context, id uuid.UUID, at *time.Time, why string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, was := m.paused[id]
	if at == nil {
		delete(m.paused, id)
		m.failing[id] = 0
		return was, nil
	}
	if was {
		return false, nil
	}
	m.paused[id] = why
	return true, nil
}

// fixture: a store, one webhook to an endpoint, and every seam held.
type fixture struct {
	mem      *memStore
	hook     uuid.UUID
	creator  uuid.UUID
	members  map[uuid.UUID]bool
	got      []*http.Request
	bodies   []string
	answer   int
	told     []string
	started  [][]int64
	at       time.Time
	endpoint *httptest.Server
	// onRequest, when set, runs as the endpoint takes each request (a test
	// that holds sends in flight). Atomic: the endpoint's goroutines read it.
	onRequest atomic.Pointer[func()]
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{mem: newMem(), hook: uuid.New(), creator: uuid.New(), answer: http.StatusOK, at: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)}
	f.members = map[uuid.UUID]bool{f.creator: true}
	var mu sync.Mutex
	f.endpoint = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if hold := f.onRequest.Load(); hold != nil {
			(*hold)()
		}
		mu.Lock()
		f.got = append(f.got, r)
		f.bodies = append(f.bodies, string(b))
		code := f.answer
		mu.Unlock()
		w.WriteHeader(code)
		_, _ = w.Write([]byte("thanks"))
	}))
	t.Cleanup(f.endpoint.Close)
	f.mem.webhooks[f.hook] = &models.Target{URL: f.endpoint.URL, Secret: "shh", CreatedBy: f.creator, Active: true}

	saved := struct {
		rows   store
		now    func() time.Time
		client interface {
			Do(*http.Request) (*http.Response, error)
		}
		checkURL     func(string) error
		isMember     func(context.Context, uuid.UUID) (bool, error)
		logTry       func(context.Context, models.Row, models.Outcome, string)
		tellPaused   func(context.Context, uuid.UUID, uuid.UUID, int, models.Outcome)
		startSending func([]int64)
	}{rows, now, client, checkURL, isMember, logTry, tellPaused, startSending}
	t.Cleanup(func() {
		rows, now, client, checkURL, isMember, logTry, tellPaused, startSending = saved.rows, saved.now, saved.client, saved.checkURL, saved.isMember, saved.logTry, saved.tellPaused, saved.startSending
	})
	rows = f.mem
	now = func() time.Time { return f.at }
	client = http.DefaultClient
	checkURL = func(string) error { return nil }
	isMember = func(_ context.Context, id uuid.UUID) (bool, error) { return f.members[id], nil }
	logTry = func(context.Context, models.Row, models.Outcome, string) {}
	tellPaused = func(_ context.Context, id, creator uuid.UUID, failing int, _ models.Outcome) {
		f.told = append(f.told, creator.String())
	}
	startSending = func(ids []int64) { f.started = append(f.started, ids) }
	return f
}

// queue writes one event for the fixture's webhook and answers its row.
func (f *fixture) queue(t *testing.T, body string) *models.Row {
	t.Helper()
	if err := Queue(context.Background(), uuid.Nil, "task.created", []Target{{WebhookID: f.hook, Payload: []byte(body)}}); err != nil {
		t.Fatal(err)
	}
	f.mem.mu.Lock()
	defer f.mem.mu.Unlock()
	return f.mem.rows[f.mem.next]
}

// tryNow claims and tries a row, as the sender does.
func (f *fixture) tryNow(t *testing.T, id int64) {
	t.Helper()
	d, ok, err := rows.claimOne(context.Background(), id, instance, lease)
	if err != nil || !ok {
		t.Fatalf("couldn't claim row %d: %v", id, err)
	}
	try(context.Background(), d)
}

func TestAnEventIsWrittenDownBeforeAnythingIsSentAndSentAtOnce(t *testing.T) {
	f := setup(t)
	other := uuid.New()
	f.mem.webhooks[other] = &models.Target{URL: f.endpoint.URL, CreatedBy: f.creator, Active: true}
	err := Queue(context.Background(), uuid.Nil, "task.created", []Target{{WebhookID: f.hook, Payload: []byte(`{"a":1}`)}, {WebhookID: other, Payload: []byte(`{"a":2}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.mem.rows) != 2 || len(f.got) != 0 {
		t.Fatalf("rows %d, sent %d: every target is written before any is sent", len(f.mem.rows), len(f.got))
	}
	if f.mem.rows[1].EventID != f.mem.rows[2].EventID || f.mem.rows[1].EventID == uuid.Nil {
		t.Fatal("one event's deliveries carry one event id")
	}
	if len(f.started) != 1 || len(f.started[0]) != 2 {
		t.Fatalf("started %v: both go at once, the drain is the net", f.started)
	}
	// A source that runs the same event again queues nothing twice.
	again := f.mem.rows[1].EventID
	if err := Queue(context.Background(), again, "task.created", []Target{{WebhookID: f.hook, Payload: []byte(`{"a":1}`)}}); err != nil {
		t.Fatal(err)
	}
	if len(f.mem.rows) != 2 {
		t.Fatalf("a repeat wrote a second delivery: %d rows", len(f.mem.rows))
	}
}

func TestADeliveryIsSignedAndCarriesItsEventIDOnEveryTry(t *testing.T) {
	f := setup(t)
	d := f.queue(t, `{"event":"task.created"}`)
	f.answer = http.StatusBadGateway
	f.tryNow(t, d.ID)
	f.answer = http.StatusOK
	f.tryNow(t, d.ID)
	if len(f.got) != 2 {
		t.Fatalf("tries sent: %d", len(f.got))
	}
	for i, r := range f.got {
		if r.Header.Get("X-OneCamp-Delivery") != d.EventID.String() {
			t.Fatalf("try %d carried delivery id %q, not the event's %s", i+1, r.Header.Get("X-OneCamp-Delivery"), d.EventID)
		}
		if f.bodies[i] != `{"event":"task.created"}` {
			t.Fatalf("try %d sent %q", i+1, f.bodies[i])
		}
		ts, sig := r.Header.Get("X-OneCamp-Timestamp"), r.Header.Get("X-OneCamp-Signature")
		if sig != "v1="+Sign("shh", ts, []byte(f.bodies[i])) {
			t.Fatalf("try %d's signature %q doesn't check out", i+1, sig)
		}
		if r.Header.Get("X-OneCamp-Retry-Num") != []string{"0", "1"}[i] || r.Header.Get("X-OneCamp-Event") != "task.created" {
			t.Fatalf("try %d headers %v", i+1, r.Header)
		}
	}
	if d.State != models.StateDone || f.mem.failing[f.hook] != 0 {
		t.Fatalf("after a 200: state %s, failing tries %d", d.State, f.mem.failing[f.hook])
	}
}

func TestAFailedTryWaitsLongerEachTimeAndIsDeadAfterTheLastTry(t *testing.T) {
	f := setup(t)
	f.answer = http.StatusInternalServerError
	d := f.queue(t, `{}`)
	var waits []time.Duration
	for i := 1; i <= MaxTries; i++ {
		f.tryNow(t, d.ID)
		if i < MaxTries {
			if d.State != models.StatePending {
				t.Fatalf("after try %d: %s", i, d.State)
			}
			waits = append(waits, d.NextAt.Sub(f.at))
		}
	}
	if d.State != models.StateDead {
		t.Fatalf("after %d failed tries: %s, not dead", MaxTries, d.State)
	}
	last := retrySchedule[len(retrySchedule)-1]
	if waits[0] < firstWait || waits[len(waits)-1] > last+last/4 {
		t.Fatalf("waits %v run from %s to at most %s and a quarter", waits, firstWait, last)
	}
	if waits[len(waits)-1] <= waits[0] {
		t.Fatalf("waits %v grow", waits)
	}
	if d.LastError == nil || *d.LastError != "It answered 500." {
		t.Fatalf("its last error: %v", d.LastError)
	}
}

func TestAWebhookThatKeepsFailingIsPausedAndItsCreatorToldOnce(t *testing.T) {
	f := setup(t)
	f.answer = http.StatusServiceUnavailable
	f.mem.failing[f.hook] = PauseAfter - 1
	d := f.queue(t, `{}`)
	f.tryNow(t, d.ID)
	if f.mem.paused[f.hook] != PausedFailing {
		t.Fatalf("after %d failed tries in a row: paused %q", PauseAfter, f.mem.paused[f.hook])
	}
	if len(f.told) != 1 || f.told[0] != f.creator.String() {
		t.Fatalf("told %v", f.told)
	}
	// Its next row waits rather than goes, and nobody is told twice.
	e := f.queue(t, `{"n":2}`)
	f.tryNow(t, e.ID)
	if len(f.got) != 1 || e.State != models.StatePending {
		t.Fatalf("a paused webhook was sent a try (%d sent), row %s", len(f.got), e.State)
	}
	if len(f.told) != 1 {
		t.Fatalf("told again: %v", f.told)
	}
}

func TestWhoIsSentWhatIsAskedAgainAsEachTryGoes(t *testing.T) {
	t.Run("a removed webhook is sent nothing more", func(t *testing.T) {
		f := setup(t)
		d := f.queue(t, `{}`)
		f.mem.webhooks[f.hook].Deleted = true
		f.tryNow(t, d.ID)
		if d.State != models.StateCancelled || len(f.got) != 0 {
			t.Fatalf("state %s, sent %d", d.State, len(f.got))
		}
	})
	t.Run("a webhook turned off is sent nothing more", func(t *testing.T) {
		f := setup(t)
		d := f.queue(t, `{}`)
		f.mem.webhooks[f.hook].Active = false
		f.tryNow(t, d.ID)
		if d.State != models.StateCancelled || len(f.got) != 0 {
			t.Fatalf("state %s, sent %d", d.State, len(f.got))
		}
	})
	t.Run("a webhook whose creator left is paused, and what it would send waits", func(t *testing.T) {
		f := setup(t)
		d := f.queue(t, `{}`)
		f.members[f.creator] = false
		f.tryNow(t, d.ID)
		if len(f.got) != 0 || d.State != models.StatePending || f.mem.paused[f.hook] != PausedOwnerGone {
			t.Fatalf("sent %d, state %s, paused %q", len(f.got), d.State, f.mem.paused[f.hook])
		}
	})
	t.Run("an address OneCamp doesn't send to is refused without a request", func(t *testing.T) {
		f := setup(t)
		checkURL = func(string) error { return errors.New("it's a private address") }
		d := f.queue(t, `{}`)
		f.tryNow(t, d.ID)
		if len(f.got) != 0 || d.LastError == nil || *d.LastError != "OneCamp doesn't send to that address: it's a private address" {
			t.Fatalf("sent %d, last error %v", len(f.got), d.LastError)
		}
	})
}

func TestAReplaySendsTheSameDeliveryAgainNow(t *testing.T) {
	f := setup(t)
	var asked []int64
	savedReplay := replayRow
	t.Cleanup(func() { replayRow = savedReplay })
	replayRow = func(_ context.Context, webhook uuid.UUID, id int64) (bool, error) {
		asked = append(asked, id)
		return webhook == f.hook && id == 7, nil
	}
	if err := Replay(context.Background(), f.hook, 7); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || len(f.started) != 1 || f.started[0][0] != 7 {
		t.Fatalf("asked %v, started %v", asked, f.started)
	}
	if err := Replay(context.Background(), uuid.New(), 7); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another webhook's delivery: %v", err)
	}
}

func TestResumingAWebhookWhoseCreatorLeftHandsItToTheAdmin(t *testing.T) {
	f := setup(t)
	admin := uuid.New()
	var took uuid.UUID
	var due bool
	savedTake, savedDue := takeOver, dueNow
	t.Cleanup(func() { takeOver, dueNow = savedTake, savedDue })
	takeOver = func(_ context.Context, _ uuid.UUID, owner uuid.UUID) error { took = owner; return nil }
	dueNow = func(context.Context, uuid.UUID) error { due = true; return nil }

	f.mem.paused[f.hook] = PausedOwnerGone
	f.members[f.creator] = false
	tookOver, err := Resume(context.Background(), f.hook, admin)
	if err != nil || !tookOver || took != admin {
		t.Fatalf("took over %v (by %s), err %v", tookOver, took, err)
	}
	if _, paused := f.mem.paused[f.hook]; paused || !due {
		t.Fatalf("still paused %v, waiting rows due %v", paused, due)
	}

	// A member's webhook stays theirs.
	f.mem.paused[f.hook] = PausedByAdmin
	f.members[f.creator] = true
	took = uuid.Nil
	if tookOver, _ := Resume(context.Background(), f.hook, admin); tookOver || took != uuid.Nil {
		t.Fatal("a member's webhook was taken over")
	}
}

func TestDeliveriesComeAPageAtATimeNewestFirst(t *testing.T) {
	saved := listRows
	t.Cleanup(func() { listRows = saved })
	listRows = func(_ context.Context, _ uuid.UUID, state string, before int64, limit int) ([]models.Row, error) {
		var out []models.Row
		for id := int64(40); id > 0 && len(out) < limit; id-- {
			if before == 0 || id < before {
				out = append(out, models.Row{ID: id, State: state})
			}
		}
		return out, nil
	}
	p, err := Deliveries(context.Background(), uuid.New(), "dead", "", 25)
	if err != nil || len(p.Items) != 25 || p.Items[0].ID != 40 || p.Next != "16" {
		t.Fatalf("first page: %d items, first %d, next %q, err %v", len(p.Items), p.Items[0].ID, p.Next, err)
	}
	p, _ = Deliveries(context.Background(), uuid.New(), "dead", p.Next, 25)
	if len(p.Items) != 15 || p.Items[0].ID != 15 || p.Next != "" {
		t.Fatalf("last page: %d items, next %q", len(p.Items), p.Next)
	}
	if _, err := Deliveries(context.Background(), uuid.New(), "lost", "", 25); err == nil {
		t.Fatal("a state that isn't one was read")
	}
	if _, err := Deliveries(context.Background(), uuid.New(), "", "page-two", 25); err == nil {
		t.Fatal("a cursor this list never gave was read")
	}
}

func TestSomeoneDeactivatedIsNoLongerAMemberAndAnUnreadAccountDecidesNothing(t *testing.T) {
	saved := activeUser
	t.Cleanup(func() { activeUser = saved })
	id := uuid.New()
	cases := []struct {
		name   string
		user   *userModels.User
		err    error
		member bool
		fails  bool
	}{
		{"a member", &userModels.User{Id: id}, nil, true, false},
		{"someone deactivated or removed (no rows)", nil, sql.ErrNoRows, false, false},
		{"an external person", &userModels.User{Id: id, IsExternal: true}, nil, false, false},
		{"a bot", &userModels.User{Id: id, IsBot: true}, nil, false, false},
		{"an account that couldn't be read", nil, errors.New("connection refused"), false, true},
	}
	for _, c := range cases {
		activeUser = func(context.Context, uuid.UUID) (*userModels.User, error) { return c.user, c.err }
		member, err := memberNow(context.Background(), id)
		if member != c.member || (err != nil) != c.fails {
			t.Errorf("%s: member %v, err %v", c.name, member, err)
		}
	}
}

// After an outage, every failed delivery of a webhook goes again in one go,
// each from its first try with its own event id.
func TestEveryFailedDeliveryGoesAgainInOneGo(t *testing.T) {
	f := setup(t)
	saved := replayDead
	t.Cleanup(func() { replayDead = saved })
	var asked uuid.UUID
	var limit int
	replayDead = func(_ context.Context, webhook uuid.UUID, n int) ([]int64, error) {
		asked, limit = webhook, n
		return []int64{3, 4, 9}, nil
	}
	n, err := ReplayFailed(context.Background(), f.hook)
	if err != nil || n != 3 {
		t.Fatalf("sent %d again, err %v", n, err)
	}
	if asked != f.hook || limit != replayAtOnce {
		t.Fatalf("asked for %s's, %d at most", asked, limit)
	}
	if len(f.started) != 1 || len(f.started[0]) != 3 || f.started[0][2] != 9 {
		t.Fatalf("started %v", f.started)
	}
	// Nothing failed: nothing starts.
	replayDead = func(context.Context, uuid.UUID, int) ([]int64, error) { return nil, nil }
	if n, err := ReplayFailed(context.Background(), f.hook); err != nil || n != 0 || len(f.started) != 1 {
		t.Fatalf("with nothing failed: %d, %v, started %v", n, err, f.started)
	}
}

// A delivery is tried quickly after a blip and then hours apart, so its last
// try comes about a day after its first: a receiver down overnight still gets
// its events.
func TestTheTriesSpanAboutADay(t *testing.T) {
	if got := helpers.ScheduleWait(retrySchedule, 1, retryJitter); got < 10*time.Second || got > 13*time.Second {
		t.Errorf("the first wait is %s", got)
	}
	var total time.Duration
	prev := time.Duration(0)
	for attempt := 1; attempt < MaxTries; attempt++ {
		base := retrySchedule[min(attempt, len(retrySchedule))-1]
		if base < prev {
			t.Errorf("wait %d (%s) is shorter than the one before (%s)", attempt, base, prev)
		}
		prev = base
		total += base
		if w := helpers.ScheduleWait(retrySchedule, attempt, retryJitter); w < base || w > base+base/4 {
			t.Errorf("wait %d is %s, outside %s plus a quarter", attempt, w, base)
		}
	}
	if total < 20*time.Hour || total > 26*time.Hour {
		t.Errorf("the last try comes %s after the first, not about a day", total)
	}
}
