package business

import (
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// CardRef is where a card lives: its host's kind ("ephemeral" for a command's
// answer shown to one person, "message" for chat's) and its id there.
type CardRef struct {
	Host string `json:"h"`
	ID   string `json:"r"`
}

// Owner is who answers a card's actions: an app (by its id) or a built-in
// handler (by its name).
type Owner struct {
	Kind string `json:"k"`
	ID   string `json:"i"`
}

// Owner kinds.
const (
	OwnerApp     = "app"
	OwnerBuiltin = "builtin"
)

// sealed is what a card's token carries: where it lives, who answers it, the
// actions it has (so no other can be pressed), who may press them (empty:
// whoever its host lets) and when it stops.
type sealed struct {
	Ref     CardRef  `json:"ref"`
	Owner   Owner    `json:"o"`
	Actions []string `json:"a"`
	Actors  []string `json:"u,omitempty"`
	Exp     int64    `json:"e"`
}

// Errors an action is refused with.
var (
	ErrBadToken  = errors.New("cards: that card isn't one of ours")
	ErrExpired   = errors.New("cards: that card has expired")
	ErrNoAction  = errors.New("cards: that card has no such action")
	ErrNotYours  = errors.New("cards: you can't act on that card")
	ErrNoOwner   = errors.New("cards: nothing answers that card's actions")
	ErrNoSecret  = errors.New("cards: the server has no signing secret")
	ErrNoTrigger = errors.New("cards: that trigger was used or has expired")
)

// cardPurpose names the key cards are sealed with: the server's secret for
// this purpose alone (helpers.PurposeKey), so a card's token is never a
// session's.
const cardPurpose = "onecamp.cards.v1"

// Seal makes the token a card is shown with: the web sends it back with an
// action, and Act trusts nothing else about the card. Sealed with the
// server's one seal (helpers.Seal) under the cards key.
func Seal(ref CardRef, owner Owner, card Card, actors []string, ttl time.Duration) (string, error) {
	s := sealed{Ref: ref, Owner: owner, Actors: actors, Exp: now().Add(ttl).Unix()}
	for _, a := range card.Actions() {
		if a.ActionID != "" {
			s.Actions = append(s.Actions, a.ActionID)
		}
	}
	token, err := helpers.Seal(cardPurpose, s)
	if errors.Is(err, helpers.ErrNoSealKey) {
		return "", ErrNoSecret
	}
	return token, err
}

// open reads a token Seal made, refusing a forged or expired one.
func open(token string) (sealed, error) {
	var s sealed
	switch err := helpers.Unseal(cardPurpose, token, &s); {
	case errors.Is(err, helpers.ErrNoSealKey):
		return sealed{}, ErrNoSecret
	case err != nil:
		return sealed{}, ErrBadToken
	}
	if now().Unix() > s.Exp {
		return sealed{}, ErrExpired
	}
	return s, nil
}

// now is the clock (a seam for tests).
var now = time.Now
