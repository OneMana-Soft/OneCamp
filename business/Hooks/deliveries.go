// Package business (Hooks) sends the workspace's events to the outgoing
// webhooks that want them, and makes sure they arrive (platform P-API part 2,
// design/platform/API_CONTRACT.md §3.3).
//
// HOW IT WAS. business/Webhook's dispatcher chose the webhooks an event goes
// to, cut each its payload (the event whole, or its ids: outgoingPrivacy.go),
// and sent it from a goroutine that tried four times in about seven seconds
// and then forgot it. A restart, a deploy or an endpoint down for a minute
// lost the event, and every try carried a new delivery id, so a receiver
// couldn't tell a retry from a second event.
//
// HOW IT IS. The dispatcher still chooses and cuts (its privacy rules are
// unchanged); Queue writes each (webhook, event) as a row before anything is
// sent, and the row is sent until it arrives:
//   - at once, by the process that queued it, and after that by the drain on
//     every worker (the Sweeper's hooks.drain, which claims its own rows);
//   - again after each failure, waiting longer each time (retrySchedule:
//     10 s at first, hours apart later), up to the Outbox's MaxAttempts tries
//     over about a day, after which it is dead: kept 30 days, shown to the
//     admin, and replayable, one at a time or all at once;
//   - with the same body and the same event id on every try and every replay
//     (X-OneCamp-Delivery, the body's event_id), signed as before (v1).
//
// Each webhook's rows go on their own, so one slow endpoint never holds back
// another. A webhook is paused by an admin, or by itself after 50 failed tries
// in a row (its creator is emailed): its rows wait, and go when it's resumed.
//
// WHO IS SENT WHAT IS DECIDED AGAIN AS EACH TRY GOES (R2): a webhook removed
// or turned off since is sent nothing more (its rows are cancelled), and one
// whose creator is no longer a member is paused, since what it sends goes out
// on its creator's say-so (the rule API tokens keep: a member's or nobody's).
// The admin who resumes it takes it over.
package business

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	webhookDomain "github.com/akashc777/OneCamp/domain/Webhook"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/WebhookDelivery"
	authService "github.com/akashc777/OneCamp/services/Auth"
	"github.com/google/uuid"
)

// The numbers.
const (
	// MaxTries is how many tries a delivery gets before it is dead: the
	// Outbox's, so the workspace's events give up at one point.
	MaxTries = outboxBusiness.MaxAttempts
	// PauseAfter is how many failed tries in a row pause a webhook.
	PauseAfter = 50
	firstWait  = 10 * time.Second
	// sendTimeout bounds one try (as business/Webhook's OutgoingTimeout did).
	sendTimeout = 10 * time.Second
	// lease is how long a claim on a row lasts: a sender that died mid-try
	// leaves it to another once it runs out.
	lease      = 2 * time.Minute
	drainEvery = 2 * time.Second
	batch      = 50
	parallel   = 8
	// loggedBody is how much of an answer is kept.
	loggedBody = 2048
	doneKeep   = 7 * 24 * time.Hour
	deadKeep   = 30 * 24 * time.Hour
	// waitLimit is how long a paused webhook's deliveries wait before they
	// are given up on.
	waitLimit = 30 * 24 * time.Hour
)

// retrySchedule is how long a delivery waits after each failed try: quick for
// a blip, then hours apart, so the last of MaxTries comes about a day after
// the first and a receiver down overnight still gets its events (Notion
// retries over about 24 hours too).
var retrySchedule = []time.Duration{
	10 * time.Second, time.Minute, 5 * time.Minute, 30 * time.Minute,
	time.Hour, 2 * time.Hour, 4 * time.Hour, 6 * time.Hour, 8 * time.Hour,
}

// retryJitter is how much more than its schedule a try may wait, at random
// (helpers.ScheduleWait), so webhooks that failed together come back apart.
const retryJitter = 0.25

// The reasons a webhook is paused.
const (
	PausedByAdmin   = "admin"
	PausedFailing   = "failing"
	PausedOwnerGone = "owner_left"
)

// SignatureVersion is the outgoing signature's version: "v1=" HMAC-SHA256 of
// "v1:<unix seconds>:<body>" in X-OneCamp-Signature, with the seconds in
// X-OneCamp-Timestamp.
const SignatureVersion = "v1"

// Sign is the v1 signature of body sent at ts (unix seconds) with secret: what
// a receiver computes to check X-OneCamp-Signature. business/Webhook checks
// incoming requests with it too, so the scheme is written once.
func Sign(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s:%s:", SignatureVersion, ts)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Target is one webhook an event goes to, with the body it is sent: the
// dispatcher's cut for it.
type Target struct {
	WebhookID uuid.UUID
	Payload   []byte
}

// ErrNotFound is a delivery or a webhook that isn't there.
var ErrNotFound = errors.New("hooks: not found")

// The seams, for tests.
type store interface {
	insert(ctx context.Context, rows []models.New) ([]int64, error)
	claimDue(ctx context.Context, by string, lease time.Duration, limit int) ([]models.Row, error)
	claimOne(ctx context.Context, id int64, by string, lease time.Duration) (models.Row, bool, error)
	finish(ctx context.Context, id int64, by, state string, o models.Outcome) (bool, error)
	retry(ctx context.Context, id int64, by string, at time.Time, o models.Outcome) (bool, error)
	target(ctx context.Context, webhookID uuid.UUID) (models.Target, bool, error)
	tried(ctx context.Context, webhookID uuid.UUID, ok bool) (int, error)
	setPaused(ctx context.Context, webhookID uuid.UUID, at *time.Time, why string) (bool, error)
}

type pgStore struct{}

func (pgStore) insert(ctx context.Context, rows []models.New) ([]int64, error) {
	return models.Insert(ctx, rows)
}
func (pgStore) claimDue(ctx context.Context, by string, l time.Duration, limit int) ([]models.Row, error) {
	return models.ClaimDue(ctx, by, l, limit)
}
func (pgStore) claimOne(ctx context.Context, id int64, by string, l time.Duration) (models.Row, bool, error) {
	return models.ClaimOne(ctx, id, by, l)
}
func (pgStore) finish(ctx context.Context, id int64, by, state string, o models.Outcome) (bool, error) {
	return models.Finish(ctx, id, by, state, o)
}
func (pgStore) retry(ctx context.Context, id int64, by string, at time.Time, o models.Outcome) (bool, error) {
	return models.Retry(ctx, id, by, at, o)
}
func (pgStore) target(ctx context.Context, id uuid.UUID) (models.Target, bool, error) {
	return models.TargetOf(ctx, id)
}
func (pgStore) tried(ctx context.Context, id uuid.UUID, ok bool) (int, error) {
	return models.Tried(ctx, id, ok)
}
func (pgStore) setPaused(ctx context.Context, id uuid.UUID, at *time.Time, why string) (bool, error) {
	return models.SetPaused(ctx, id, at, why)
}

var (
	rows store = pgStore{}
	now        = time.Now
	// client sends the tries: SSRF-safe, pinning the address it resolved.
	client interface {
		Do(*http.Request) (*http.Response, error)
	} = helpers.SSRFSafeClient(false)
	// checkURL refuses an address OneCamp doesn't send to.
	checkURL = func(raw string) error { _, err := helpers.ValidateOutboundURL(raw, false); return err }
	// activeUser reads a person's account as it is now: no rows for one who
	// was deactivated or removed.
	activeUser = userDomain.GetActiveUserWithAdminFlagByUserUUID
	// isMember says whether a webhook's creator is still a member.
	isMember = memberNow
	// logTry writes a try into the webhook's log (Admin › Webhooks › Logs).
	logTry = func(ctx context.Context, d models.Row, o models.Outcome, answer string) {
		var status *int
		if o.Status != nil {
			status = o.Status
		}
		body := string(d.Payload)
		var errText *string
		if o.Error != "" {
			errText = &o.Error
		}
		ok := o.Error == "" && status != nil && *status >= 200 && *status < 300
		ms := o.Ms
		var ans *string
		if answer != "" {
			ans = &answer
		}
		_ = webhookDomain.CreateWebhookLog(ctx, d.WebhookID, d.EventType, &body, status, ans, errText, &ms, ok)
	}
	// tellPaused emails a webhook's creator that it paused itself.
	tellPaused = tellCreator
	// startSending starts the rows a Queue or a Replay wrote on their way.
	startSending = func(ids []int64) { SendNow(ids) }
)

// memberNow says whether id is a member now: false for someone deactivated or
// removed (their account reads as no rows), an external person or a bot; an
// error only when it couldn't be read, which decides nothing.
func memberNow(ctx context.Context, id uuid.UUID) (bool, error) {
	u, err := activeUser(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return u != nil && u.IsMember(), nil
}

// instance names this process in the claims it takes.
var instance = func() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s/%d/%s", host, os.Getpid(), uuid.NewString()[:8])
}()

// Queue writes one delivery per target for the event (one a target has
// already, as when a source runs the same event again, is left as it is) and
// starts sending them; the drain is the net under that.
func Queue(ctx context.Context, eventID uuid.UUID, eventType string, targets []Target) error {
	if len(targets) == 0 {
		return nil
	}
	if eventID == uuid.Nil {
		eventID = uuid.New()
	}
	news := make([]models.New, 0, len(targets))
	for _, t := range targets {
		news = append(news, models.New{WebhookID: t.WebhookID, EventID: eventID, EventType: eventType, Payload: t.Payload})
	}
	ids, err := rows.insert(ctx, news)
	if err != nil {
		return fmt.Errorf("hooks: couldn't queue %s for %d webhooks: %w", eventType, len(targets), err)
	}
	startSending(ids)
	return nil
}

// sending bounds the tries this process makes at once outside the drain.
var sending = make(chan struct{}, 100)

// SendNow tries the rows ids now, each on its own goroutine, bound to the
// server's life rather than the request's, as many as the pool has places
// for; the rest wait for the drain. One another sender has, or that isn't
// due, is left alone.
func SendNow(ids []int64) {
	for _, id := range ids {
		id := id
		// A place before the goroutine, never after: with the pool full the
		// delivery stays queued for the drain (its row is in Postgres), so a
		// burst of events neither parks a goroutine per delivery nor holds up
		// the request that queued them.
		helpers.TryGoSafeBounded(sending, "hooks.send", func() {
			ctx, cancel := context.WithTimeout(context.Background(), lease)
			defer cancel()
			d, ok, err := rows.claimOne(ctx, id, instance, lease)
			if err != nil {
				helpers.LogErrorWithContext(ctx, "hooks: couldn't claim delivery %d to send it; the drain will: %v", id, err)
				return
			}
			if ok {
				try(ctx, d)
			}
		})
	}
}

// Drain sends the due rows: the Sweeper's hooks.drain on every worker, each
// claiming its own.
func Drain(ctx context.Context) error {
	due, err := rows.claimDue(ctx, instance, lease, batch)
	if err != nil {
		return fmt.Errorf("hooks: couldn't claim due deliveries: %w", err)
	}
	var wg sync.WaitGroup
	slots := make(chan struct{}, parallel)
	for _, d := range due {
		d := d
		wg.Add(1)
		helpers.GoSafeBounded(slots, "hooks.drain", func() {
			defer wg.Done()
			try(ctx, d)
		})
	}
	wg.Wait()
	return nil
}

// try makes one try of a claimed row and records what came of it.
func try(ctx context.Context, d models.Row) {
	t, ok, err := rows.target(ctx, d.WebhookID)
	if err != nil {
		// Not known is not refused: the row goes back, untried.
		_, _ = rows.retry(ctx, d.ID, instance, now().Add(firstWait), models.Outcome{Error: "Couldn't read the webhook; it will be tried again."})
		return
	}
	switch {
	case !ok || t.Deleted:
		_, _ = rows.finish(ctx, d.ID, instance, models.StateCancelled, models.Outcome{Error: "The webhook was removed."})
		return
	case !t.Active:
		_, _ = rows.finish(ctx, d.ID, instance, models.StateCancelled, models.Outcome{Error: "The webhook was turned off."})
		return
	case t.PausedAt != nil:
		// Paused since it was claimed: it waits with the rest.
		_, _ = rows.retry(ctx, d.ID, instance, now().Add(firstWait), models.Outcome{Error: "The webhook is paused."})
		return
	}
	if member, err := isMember(ctx, t.CreatedBy); err == nil && !member {
		at := now()
		if changed, _ := rows.setPaused(ctx, d.WebhookID, &at, PausedOwnerGone); changed {
			helpers.LogInfoWithContext(ctx, "hooks: webhook %s paused: its creator is no longer a member", d.WebhookID)
		}
		_, _ = rows.retry(ctx, d.ID, instance, now().Add(firstWait), models.Outcome{Error: "Its creator is no longer a member: an admin can resume it and take it over."})
		return
	}

	o, answer := send(ctx, d, t)
	logTry(ctx, d, o, answer)
	tryDuration.WithLabelValues(d.EventType).Observe(float64(o.Ms) / 1000)
	if o.Error == "" {
		tries.WithLabelValues(d.EventType, "success").Inc()
		_, _ = rows.finish(ctx, d.ID, instance, models.StateDone, o)
		_, _ = rows.tried(ctx, d.WebhookID, true)
		return
	}
	tries.WithLabelValues(d.EventType, "failure").Inc()
	if d.Attempts >= MaxTries {
		gaveUp.WithLabelValues(d.EventType).Inc()
		_, _ = rows.finish(ctx, d.ID, instance, models.StateDead, o)
	} else {
		retries.WithLabelValues(d.EventType).Inc()
		_, _ = rows.retry(ctx, d.ID, instance, now().Add(helpers.ScheduleWait(retrySchedule, d.Attempts, retryJitter)), o)
	}
	failing, err := rows.tried(ctx, d.WebhookID, false)
	if err == nil && failing >= PauseAfter {
		at := now()
		if changed, _ := rows.setPaused(ctx, d.WebhookID, &at, PausedFailing); changed {
			tellPaused(ctx, d.WebhookID, t.CreatedBy, failing, o)
		}
	}
}

// send posts a row's body to its webhook, signed, and answers how it went and
// the start of what the endpoint said.
func send(ctx context.Context, d models.Row, t models.Target) (models.Outcome, string) {
	if t.URL == "" {
		return models.Outcome{Error: "The webhook has no address to send to."}, ""
	}
	if err := checkURL(t.URL); err != nil {
		return models.Outcome{Error: "OneCamp doesn't send to that address: " + err.Error()}, ""
	}
	cctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, t.URL, bytes.NewReader(d.Payload))
	if err != nil {
		return models.Outcome{Error: "The webhook's address isn't one OneCamp can send to."}, ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "OneCamp-Webhook/1.0")
	req.Header.Set("X-OneCamp-Event", d.EventType)
	req.Header.Set("X-OneCamp-Webhook-Id", d.WebhookID.String())
	// The same on every try and every replay, so a receiver can tell a retry
	// from a new event.
	req.Header.Set("X-OneCamp-Delivery", d.EventID.String())
	req.Header.Set("X-OneCamp-Retry-Num", strconv.Itoa(max(d.Attempts-1, 0)))
	if t.Secret != "" {
		ts := strconv.FormatInt(now().Unix(), 10)
		req.Header.Set("X-OneCamp-Signature", SignatureVersion+"="+Sign(t.Secret, ts, d.Payload))
		req.Header.Set("X-OneCamp-Timestamp", ts)
	}
	start := now()
	resp, err := client.Do(req)
	ms := int(now().Sub(start).Milliseconds())
	if err != nil {
		return models.Outcome{Error: "Couldn't reach it: " + err.Error(), Ms: ms}, ""
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, loggedBody))
	status := resp.StatusCode
	o := models.Outcome{Status: &status, Ms: ms}
	if status < 200 || status >= 300 {
		o.Error = fmt.Sprintf("It answered %d.", status)
	}
	return o, string(answer)
}

// tellCreator emails a webhook's creator that it paused itself, once a pause.
func tellCreator(ctx context.Context, webhookID, creator uuid.UUID, failing int, last models.Outcome) {
	w, err := webhookDomain.GetWebhookById(ctx, webhookID)
	if err != nil || w == nil {
		return
	}
	notificationBusiness.Dispatch(ctx, notificationBusiness.Event{
		Type:            "webhook.paused",
		SubjectLine:     fmt.Sprintf("Your webhook %q is paused", w.Name),
		Title:           "A webhook paused itself",
		Subtitle:        w.Name,
		Body:            fmt.Sprintf("Its last %d tries failed (the last: %s). What it would have sent is waiting, and goes when you resume it in Admin, Webhooks.", failing, last.Error),
		CTAURL:          authService.FrontendBaseURL() + "/app/admin?tab=webhooks",
		CTAText:         "Open webhooks",
		DedupKeyParts:   []string{"webhook.paused", webhookID.String(), now().UTC().Format(time.RFC3339)},
		Recipients:      []notificationBusiness.Recipient{{UserUUID: creator}},
		SkipOnlineCheck: true,
	})
}

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "hooks.drain", Every: drainEvery, ClaimsItsOwnWork: true, Run: Drain})
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "hooks.purge", Every: time.Hour, Run: purge})
}
