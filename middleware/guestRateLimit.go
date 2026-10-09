package middleware

// Writes through a guest link (a client reviewing a project, a contractor in a
// shared channel) are limited per address, so a leaked link can't be used to
// flood a channel or a doc. They used the sign-in limit, 20 in 15 minutes
// answered "Too many login attempts", and most pages shared one bucket: a
// client approving a sprint's tasks ran out before they could comment, and was
// told they had tried to sign in too often. Each kind of guest write now has
// its own bucket, sized for a busy session from one office's address, and the
// answer says what happened.
//
// They are limited per link too. A per-address limit alone let a leaked link,
// used from many addresses, write as much as it liked; the link's own bucket
// is twice an address's, so the people a link was shared with, from a few
// offices, never meet it.

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// GuestWrite is a kind of write a guest link allows; each has its own bucket.
type GuestWrite string

const (
	GuestMessage    GuestWrite = "message"     // in a shared channel, or a thread of it
	GuestComment    GuestWrite = "comment"     // on a task of a shared project
	GuestApproval   GuestWrite = "approval"    // approving a task, or asking for changes
	GuestDocComment GuestWrite = "doc-comment" // on a shared doc
	GuestJoin       GuestWrite = "join"        // joining a meeting
	GuestOpenLive   GuestWrite = "open-live"   // opening a live doc or board, again on each reconnect
)

// guestWriteCaps is how many of each one address may make in the limiter's
// 15-minute window: one every few seconds, kept up for the whole window.
var guestWriteCaps = map[GuestWrite]int{
	GuestMessage:    150,
	GuestComment:    150,
	GuestApproval:   150,
	GuestDocComment: 150,
	GuestJoin:       30,
	GuestOpenLive:   120,
}

// guestLinkCaps is how many of each one link may take in the same window,
// from all its addresses together. Joining a meeting and opening a live doc
// or board have none: they write nothing a leaked link could flood with, and
// they count readers (a live page fetches its token again on each reconnect),
// so a big guest meeting, or the collaboration service blinking, locked every
// guest of the link out for fifteen minutes. Each address keeps its cap.
var guestLinkCaps = map[GuestWrite]int{
	GuestMessage:    300,
	GuestComment:    300,
	GuestApproval:   300,
	GuestDocComment: 300,
}

// guestRateMsg is what a guest who reached a cap is told.
const guestRateMsg = "Too many requests from here. Wait a few minutes and try again."

// guestLinkRateMsg is what everyone on a link that reached its cap is told.
const guestLinkRateMsg = "Too many requests through this link. Wait a few minutes and try again."

// GuestRateLimit limits one kind of guest write per address, and per link.
func GuestRateLimit(kind GuestWrite) func(http.Handler) http.Handler {
	byAddress := IPRateLimit("guest-"+string(kind), guestWriteCaps[kind], guestRateMsg)
	if _, perLink := guestLinkCaps[kind]; !perLink {
		return byAddress
	}
	return func(next http.Handler) http.Handler {
		return byAddress(guestLinkLimit(kind, next))
	}
}

// guestLinkLimit counts one kind of guest write against the link in the URL
// ({token}), by a hash of it: the token itself is a credential and is never a
// key. Counted in this process when Redis can't be asked, as IPRateLimit does.
func guestLinkLimit(kind GuestWrite, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := chi.URLParam(r, "token")
		if r.Method == http.MethodOptions || r.Method == http.MethodGet || token == "" {
			next.ServeHTTP(w, r)
			return
		}
		sum := sha256.Sum256([]byte(token))
		rctx, cancel := contextWithRedisTimeout(r.Context())
		defer cancel()
		res := redisStore.AllowFixedWindowOrLocal(rctx, registry.LoginRate,
			[]string{"guest-link-" + string(kind), hex.EncodeToString(sum[:12])}, guestLinkCaps[kind])
		if !res.Allowed {
			retryAfter := res.RetryAfterSeconds()
			if retryAfter <= 0 {
				retryAfter = int(registry.LoginRate.TTL.Seconds())
			}
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": guestLinkRateMsg, "status": "rate_limited"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
