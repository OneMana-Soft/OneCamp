package business

// What a password gives (D-25, condition 2; shared by every kind of address
// since docs C6-2, agreed with CALLS-LEAD): a short-lived token for one
// grant at its unlock version, carried in the address (?k=), never in a
// cookie, so a page reads no cookie and its bytes are the same for everyone
// who holds the address. A new password, or a new address, moves the version
// and ends every token given before. Each kind signs under a key of its own
// (Kind.unlockLabel), and the grant is in the signature, so a token opens
// its own grant and no other, of any kind.
//
// A password is tried at most unlockTriesPerWindow times from one address
// for one grant in the limit's window, whichever kind's address it was tried
// at, and the grant has a ceiling over every address together
// (UnlockTryAllowed).

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"strconv"
	"strings"
	"time"

	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// Password tries in the limits' fifteen-minute window: unlockTriesPerWindow
// per address and grant, and unlockTriesPerGrant at one grant from every
// address together (audit A8). Past that ceiling the grant isn't shut, or a
// stranger could keep its real visitors out: each address gets
// unlockTriesPastCeiling more, so a guess spread over many addresses slows
// tenfold while a visitor who knows the password still gets in.
const (
	unlockTriesPerWindow   = 10
	unlockTriesPerGrant    = 100
	unlockTriesPastCeiling = 1
)

// Seams for tests: the signing secret and the tries' counter (one try under
// a limit's key, within its count).
var (
	unlockSecret = func() string { return os.Getenv("JWT_SECRET") }
	triesAllowed = func(ctx context.Context, spec registry.Spec, args []string, limit int) bool {
		return redisStore.AllowFixedWindowOrLocal(ctx, spec, args, limit).Allowed
	}
)

// unlockKey is the key a kind's unlocks are signed with: derived from the
// server's signing secret under the kind's label, kept apart from every other
// use of the secret; nil when the server has none.
func (k Kind) unlockKey() []byte {
	secret := unlockSecret()
	if secret == "" {
		return nil
	}
	label := k.unlockLabel
	if label == "" {
		label = "onecamp/guest/unlock/" + k.Name
	}
	key := sha256.Sum256([]byte(label + "\x00" + secret))
	return key[:]
}

func signUnlock(key []byte, grant string, version int, exp int64) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(grant + "\x00" + strconv.Itoa(version) + "\x00" + strconv.FormatInt(exp, 10)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// MintUnlock is a token that opens g, at its unlock version, for its kind's
// UnlockFor from now, and how long that is; "" when the server has no signing
// secret or the kind gives no unlocks.
func MintUnlock(g *guestModel.GuestGrant, now time.Time) (string, time.Duration) {
	if g == nil {
		return "", 0
	}
	k := KindOf(g)
	key := k.unlockKey()
	if key == nil || k.UnlockFor <= 0 {
		return "", 0
	}
	exp := now.Add(k.UnlockFor).Unix()
	return strconv.Itoa(g.UnlockVersion) + "." + strconv.FormatInt(exp, 10) + "." + signUnlock(key, g.Id.String(), g.UnlockVersion, exp), k.UnlockFor
}

// Unlocks reports whether token opens g now: made for g, under its kind, at
// its unlock version, and not run out.
func Unlocks(token string, g *guestModel.GuestGrant, now time.Time) bool {
	if g == nil || token == "" || len(token) > 200 {
		return false
	}
	key := KindOf(g).unlockKey()
	if key == nil {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	v, err := strconv.Atoi(parts[0])
	if err != nil || v != g.UnlockVersion {
		return false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || now.Unix() >= exp {
		return false
	}
	return hmac.Equal([]byte(parts[2]), []byte(signUnlock(key, g.Id.String(), g.UnlockVersion, exp)))
}

// UnlockTryAllowed counts one password try at g from ip (helpers.RateKeyIP's
// key), and reports whether it may be tried. The address's own tries come
// first, so an address past them never counts against the grant.
func UnlockTryAllowed(ctx context.Context, ip string, g *guestModel.GuestGrant) bool {
	if g == nil {
		return false
	}
	grant := g.Id.String()
	if !triesAllowed(ctx, registry.PublishUnlockRate, []string{ip, grant}, unlockTriesPerWindow) {
		return false
	}
	if triesAllowed(ctx, registry.PublishUnlockGrantRate, []string{grant}, unlockTriesPerGrant) {
		return true
	}
	return triesAllowed(ctx, registry.PublishUnlockSlowRate, []string{ip, grant}, unlockTriesPastCeiling)
}
