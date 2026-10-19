package middleware

// Reads of a page anyone may open are limited too. IPRateLimit counts only
// writes, which suits a guess at a password but not a read that costs: a
// booking page's free times are its owner's calendars, Google's included, read
// with the owner's own token. Nothing counted GET /public/book/{slug}, so a
// script could ask for it as fast as it liked, spending the owner's Google
// quota (their calendar then showed no Google events) and loading the server.
//
// publicReadLimit counts every request per address (an IPv6 address by its
// /64, helpers.RateKeyIP) in a one-minute window (registry.PublicReadRate),
// counted in this process when Redis can't be asked, as IPRateLimit does.
// A booking page's own bucket, for a flood from many addresses, is counted in
// business/Calendar, and only when an answer has to be worked out afresh: a
// bucket here counted answers served from the cache too, for every visitor
// together, so one host could keep a page at 429 for everyone.

import (
	"net/http"

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

// publicReadLimit limits requests of every method (preflights aside) to
// perAddress a minute from one address.
func publicReadLimit(kind string, perAddress int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}
			if ip := helpers.ClientIP(r); ip != "" {
				ctx, cancel := contextWithRedisTimeout(r.Context())
				defer cancel()
				res := redisStore.AllowFixedWindowOrLocal(ctx, registry.PublicReadRate, []string{kind, helpers.RateKeyIP(ip)}, perAddress)
				if !res.Allowed {
					WriteRateLimited(w, res, registry.PublicReadRate, publicReadAddressMsg)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
