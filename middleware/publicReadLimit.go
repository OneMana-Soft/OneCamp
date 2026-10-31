package middleware

// Reads of a page anyone may open are limited too. IPRateLimit counts only
// writes, which suits a guess at a password but not a read that costs: a
// booking page's free times are its owner's calendars, Google's included, read
// with the owner's own token. Nothing counted GET /public/book/{slug}, so a
// script could ask for it as fast as it liked, spending the owner's Google
// quota (their calendar then showed no Google events) and loading the server.
//
// publicReadLimit counts every request per address (redisStore.AllowPerAddress,
// the one limit every public read shares), counted in this process when Redis
// can't be asked, as IPRateLimit does.
// A booking page's own bucket, for a flood from many addresses, is counted in
// business/Calendar, and only when an answer has to be worked out afresh: a
// bucket here counted answers served from the cache too, for every visitor
// together, so one host could keep a page at 429 for everyone.

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

const (
	publicReadAddressMsg = "Too many requests from here. Wait a minute and try again."

	// A visitor's booking page asks for a fortnight, then two requests a
	// month they look at: sixty a minute is a person clicking as fast as
	// they can, and more is a script.
	bookingPageReadsPerAddress = 60

	// A guest's open call chat asks every 5 s while it's in view (calls
	// CALLS-3b), twelve a minute: 120 holds ten guests behind one address
	// (a meeting room's) and turns a script away.
	callThreadReadsPerAddress = 120
	// A guest's call reads its polls and questions again when told they
	// changed (calls package A), at most once in 2 s: 300 holds ten guests
	// behind one address through a busy Q&A.
	callActivitiesReadsPerAddress = 300
)

// BookingPageReadLimit limits GET /public/book/{slug}: a booking page's free
// times, per address.
func BookingPageReadLimit() func(http.Handler) http.Handler {
	return publicReadLimit("booking-page", bookingPageReadsPerAddress)
}

// BookingLinkReadLimit limits GET /public/booking/{token}: a guest's booking,
// per address. Its token is 24 random bytes, so this bounds load, not guessing.
func BookingLinkReadLimit() func(http.Handler) http.Handler {
	return publicReadLimit("booking-link", bookingPageReadsPerAddress)
}

// InvitationReadLimit limits GET /public/rsvp/{token}: a guest's invitation to
// an event, per address (calendar A2). Its token is 32 random bytes, so this
// bounds load, not guessing.
func InvitationReadLimit() func(http.Handler) http.Handler {
	return publicReadLimit("invitation", bookingPageReadsPerAddress)
}

// A link's page (a guest's meeting and its lobby, table, channel, project or
// doc comments; a form; an unsubscribe link) is read by a client's team, often
// from one office address, with the page polling. The busiest is a guest
// channel: the page every 5 s and an open thread every 5 s too
// (components/guest GUEST_POLL_MS), 24 a minute a person in one bucket, so 600
// fits a team of 25; a project costs 10 a minute a person, a meeting's green
// room 6. It is still 10 a second per surface from one address. IPRateLimit
// and LoginRateLimit, on some of these routes, count their reads at this
// budget too.
const linkReadsPerAddress = 600

// A file on a public or guest page (a board's attachments, an app's icon)
// comes many to a page.
const fileReadsPerAddress = 1200

// LinkReadLimit limits the reads of a link's page of kind, per address.
func LinkReadLimit(kind string) func(http.Handler) http.Handler {
	return publicReadLimit(kind, linkReadsPerAddress)
}

// FileReadLimit limits the reads of a public file of kind, per address.
func FileReadLimit(kind string) func(http.Handler) http.Handler {
	return publicReadLimit(kind, fileReadsPerAddress)
}

// A member's collaboration token is asked for again by each open doc or board
// tab about every five minutes, and an office's tabs share one address and
// come back together after a deploy or at nine; behind a proxy that hides its
// clients, one address is the whole workspace.
const collabTokenReadsPerAddress = 1200

// CollabTokenReadLimit limits the reads of GET /auth/token, per address.
func CollabTokenReadLimit() func(http.Handler) http.Handler {
	return publicReadLimit("collab", collabTokenReadsPerAddress)
}

// CallThreadReadLimit limits GET /guest/calls/thread: a guest's read of the
// call's chat, per address. Its call token is signed, so this bounds load,
// not guessing.
func CallThreadReadLimit() func(http.Handler) http.Handler {
	return publicReadLimit("call-thread", callThreadReadsPerAddress)
}

// CallActivitiesReadLimit limits GET /guest/calls/activities: a guest's read
// of the call's polls and questions, per address.
func CallActivitiesReadLimit() func(http.Handler) http.Handler {
	return publicReadLimit("call-activities", callActivitiesReadsPerAddress)
}

// publicReadLimit limits requests of every method (preflights aside) to
// perAddress a minute from one address.
func publicReadLimit(kind string, perAddress int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}
			if res := redisStore.AllowPerAddress(r.Context(), kind, helpers.ClientIP(r), perAddress); !res.Allowed {
				WriteRateLimited(w, res, registry.PublicReadRate, publicReadAddressMsg)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// A calendar's secret link (calendar B8) is read by another calendar app every
// few minutes at most, so twenty a minute is a client gone wrong; Google's and
// Apple's readers fetch many people's links from the same few addresses, so
// an address may read many.
const (
	calendarFeedReadsPerLink    = 20
	calendarFeedReadsPerAddress = 600
)

// CalendarFeedReadLimit limits GET /public/calendar/{file}: each link by its
// token's hash (never the token itself), and each address loosely.
func CalendarFeedReadLimit() func(http.Handler) http.Handler {
	perAddress := publicReadLimit("calendar-feed", calendarFeedReadsPerAddress)
	return func(next http.Handler) http.Handler {
		perLink := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodOptions {
				sum := sha256.Sum256([]byte(chi.URLParam(r, "file")))
				ctx, cancel := contextWithRedisTimeout(r.Context())
				defer cancel()
				res := redisStore.AllowFixedWindowOrLocal(ctx, registry.PublicReadRate, []string{"calendar-feed-link", hex.EncodeToString(sum[:12])}, calendarFeedReadsPerLink)
				if !res.Allowed {
					WriteRateLimited(w, res, registry.PublicReadRate, publicReadAddressMsg)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
		return perAddress(perLink)
	}
}
