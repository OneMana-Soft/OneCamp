package business

// A call's guest on the board on the call's stage (calls package B). Their
// call token, which proves they are in the call, buys a collab token for that
// one board (IssueCallBoardToken). The collaboration service asks again on
// every connect (AuthorizeGuestCallBoard): the board must still be on the
// stage of that call and they must still be in it, as the one board grant
// (business/CallState BoardGrant) says, and they draw only under the
// presenter's "Guests can draw" (BoardAccess). The token is never a share
// link's: neither path opens the other's.

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	"github.com/akashc777/OneCamp/helpers"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/golang-jwt/jwt/v5"
)

// ErrNotCallToken is a token that names no call: a share link's (the raw
// token on its pictures' path, or its collab token), for the link path.
var ErrNotCallToken = errors.New("not a call's board token")

// callBoardClaims is the verified payload of a call's board token.
type callBoardClaims struct {
	// Identity is the guest's call identity, "guest-<grant id>-<random>".
	Identity string
	// Call is the call's session id.
	Call  string
	Board string
	Name  string
}

// IssueCallBoardToken mints the collab token a call's guest (identity, named
// name in the call) opens board with while it is on the stage of the call
// sessionID. The caller has checked they are in the call now.
func IssueCallBoardToken(ctx context.Context, identity, name, sessionID, board string) (string, error) {
	if grantOfGuest(identity) == "" || sessionID == "" || board == "" {
		return "", ErrInvalidGrant
	}
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		return "", errors.New("server auth not configured")
	}
	shown := sanitizeGuestName(NameInCall(name))
	if shown == "" {
		shown = "Guest"
	}
	now := time.Now()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":   identity,
		"name":  callNamePrefix + shown,
		"guest": true,
		"call":  sessionID,
		"rt":    guestModel.ResourceBoard,
		"rid":   board,
		"iat":   now.Unix(),
		"exp":   now.Add(guestCollabTokenTTL).Unix(),
	}).SignedString([]byte(secret))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Guest/IssueCallBoardToken sign err: %+v", err)
		return "", err
	}
	return signed, nil
}

// AuthorizeGuestCallBoard answers whether a call's guest draws on board, or
// ErrInvalidGrant when the call doesn't let them open it now, ErrNotFound for
// a deleted board, and ErrNotCallToken for a token that isn't a call's.
func AuthorizeGuestCallBoard(ctx context.Context, bearerToken, board string) (bool, error) {
	c, edit, err := openCallBoard(ctx, bearerToken)
	if err != nil {
		return false, err
	}
	if c.Board != strings.TrimSpace(board) {
		return false, ErrInvalidGrant
	}
	return edit, nil
}

// GuestCallBoardOf answers the board a call's board token opens now (for its
// pictures), with AuthorizeGuestCallBoard's refusals.
func GuestCallBoardOf(ctx context.Context, token string) (string, error) {
	c, _, err := openCallBoard(ctx, token)
	if err != nil {
		return "", err
	}
	return c.Board, nil
}

// openCallBoard verifies a call's board token and asks the call, now, whether
// its guest may open the board it names, and draw on it.
func openCallBoard(ctx context.Context, token string) (*callBoardClaims, bool, error) {
	c, err := parseCallBoardClaims(strings.TrimSpace(token), os.Getenv("JWT_SECRET"))
	if err != nil {
		return nil, false, err
	}
	live, err := boardBusiness.Live(ctx, c.Board)
	if err != nil {
		return nil, false, err
	}
	if !live {
		return nil, false, ErrNotFound
	}
	grant, err := callStateBusiness.BoardGrant(ctx, c.Board, c.Identity)
	if err != nil {
		return nil, false, err
	}
	edit, err := callBoardEdit(c, c.Board, grant)
	return c, edit, err
}

// callBoardEdit is whether the guest c draws on board, given what a call they
// are in now grants them on it (nil for nothing): the grant must be theirs, as
// a guest, from the call and for the board their token names. Pure.
func callBoardEdit(c *callBoardClaims, board string, grant *callStateBusiness.Grant) (bool, error) {
	if grant == nil || !grant.Guest || grant.Identity != c.Identity || grant.SessionID != c.Call ||
		grant.Board.UUID != board || c.Board != board {
		return false, ErrInvalidGrant
	}
	_, edit := callStateBusiness.BoardAccess(false, false, grant, false)
	return edit, nil
}

// parseCallBoardClaims verifies a call's board token. A token that names no
// call is a share link's (ErrNotCallToken), read before the signature: that
// only picks the path, the link's path judges it, and a link never needs this
// server's secret. A token that names one is the call's alone to refuse.
func parseCallBoardClaims(tokenString, secret string) (*callBoardClaims, error) {
	named := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(tokenString, named); err != nil {
		return nil, ErrNotCallToken
	}
	if call, _ := named["call"].(string); call == "" {
		return nil, ErrNotCallToken
	}
	if secret == "" {
		return nil, errors.New("server auth not configured")
	}
	claims, err := verifiedGuestClaims(tokenString, secret)
	if err != nil {
		return nil, err
	}
	call, _ := claims["call"].(string)
	sub, _ := claims["sub"].(string)
	rt, _ := claims["rt"].(string)
	rid, _ := claims["rid"].(string)
	name, _ := claims["name"].(string)
	if _, link := claims["gid"]; link || grantOfGuest(sub) == "" || rt != guestModel.ResourceBoard || rid == "" {
		return nil, ErrInvalidGrant
	}
	return &callBoardClaims{Identity: sub, Call: call, Board: rid, Name: name}, nil
}
