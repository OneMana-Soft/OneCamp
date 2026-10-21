package business

// Response URLs (#33, API_CONTRACT.md §3.4): an app's command is forwarded
// with a response_url it may answer at later, up to ResponseUses times within
// ResponseLife, as Slack's do. An answer is ephemeral (only the person who
// typed the command sees it) unless it asks to be posted, which the app's own
// user does, in the channel the command was typed in (never a direct message
// or group: an app isn't one of the people in it).
//
// Before these, an app had one chance to answer: the body of its reply to the
// forward, within ten seconds. Anything slower (a deploy, a search, a report)
// could only answer through a webhook of its own.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	botpost "github.com/akashc777/OneCamp/business/BotPost"
	cardsBusiness "github.com/akashc777/OneCamp/business/Cards"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	authService "github.com/akashc777/OneCamp/services/Auth"
	"github.com/google/uuid"
)

// The numbers (§3.4).
const (
	ResponseUses = 5
	ResponseLife = 30 * time.Minute
	// cardLife is how long a card an app answers with can be pressed.
	cardLife = 24 * time.Hour
)

// Answers a response URL is refused with.
var (
	ErrResponseGone = errors.New("this response URL has expired or has been used five times")
	ErrNoPlace      = errors.New("an app posts only in a channel; answer this one ephemerally")
)

// invocation is what a response URL answers for: the command, who typed it,
// where, and the app that owns it.
type invocation struct {
	AppID     string `json:"app_id"`
	AppName   string `json:"app_name"`
	AppIcon   string `json:"app_icon,omitempty"`
	Command   string `json:"command"`
	UserUUID  string `json:"user_uuid"`
	ChannelID string `json:"channel_id,omitempty"`
	DmGroupID string `json:"dm_group_id,omitempty"`
	ThreadTs  string `json:"thread_ts,omitempty"`
	TriggerID string `json:"trigger_id,omitempty"`
	Uses      int    `json:"uses"`
	// Expires is when it stops (unix seconds), counted from the command, not
	// from its last answer: a use rewrites the key, and with it the key's TTL.
	Expires int64 `json:"expires"`
}

// RespondBody is an app's answer at its response URL, in Slack's shape.
type RespondBody struct {
	Text            string          `json:"text"`
	Blocks          json.RawMessage `json:"blocks,omitempty"`
	ResponseType    string          `json:"response_type"`
	ReplaceOriginal bool            `json:"replace_original"`
}

// The seams, for tests.
var (
	saveInvocation = func(ctx context.Context, token string, inv invocation) error {
		return redisStore.SetJSONWithTTL(ctx, registry.CommandResponse, []string{token}, inv, ResponseLife)
	}
	// takeUse spends one of a response URL's uses, answering what it is for.
	// A token that was never made writes nothing (the atomic update would).
	takeUse = func(ctx context.Context, token string) (invocation, bool, error) {
		var seen invocation
		if found, err := redisStore.GetJSON(ctx, registry.CommandResponse, []string{token}, &seen); err != nil || !found {
			return invocation{}, false, err
		}
		var got invocation
		var ok bool
		err := redisStore.UpdateJSONAtomic(ctx, registry.CommandResponse, []string{token}, ResponseLife,
			func(cur invocation, found bool) invocation {
				if !found || cur.Uses <= 0 || time.Now().Unix() > cur.Expires {
					return cur
				}
				cur.Uses--
				got, ok = cur, true
				return cur
			})
		return got, ok, err
	}
	deliverEphemeral = func(ctx context.Context, userUUID string, resp *commandAdapter.CommandResponse) {
		var u userModels.UserInfo
		u.UserDgraphInfo.Uuid = userUUID
		deliverAsync(ctx, CommandContext{User: u}, resp)
	}
	postInChannel = func(ctx context.Context, inv invocation, text string) error {
		channel, err := uuid.Parse(inv.ChannelID)
		if err != nil {
			return ErrNoPlace
		}
		appID, err := uuid.Parse(inv.AppID)
		if err != nil {
			return fmt.Errorf("the app's id: %w", err)
		}
		bot, err := userBusiness.EnsureAppBot(ctx, appID, inv.AppName, inv.AppIcon)
		if err != nil {
			return err
		}
		_, err = botpost.PostToChannelAsBot(ctx, channel, text, bot)
		return err
	}
	responseBase = func() string { return authService.BackendBaseURL() }
)

// newResponseURL is the response URL an app's command is forwarded with, or ""
// when one can't be kept (no Redis): the app then answers in its reply alone.
func newResponseURL(ctx context.Context, appID uuid.UUID, appName, appIcon string, cc CommandContext) string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	token := hex.EncodeToString(b)
	inv := invocation{AppID: appID.String(), AppName: appName, AppIcon: appIcon, Command: cc.Command, UserUUID: cc.User.UserDgraphInfo.Uuid, TriggerID: cc.TriggerID, Uses: ResponseUses, Expires: time.Now().Add(ResponseLife).Unix()}
	if cc.ChannelID != nil {
		inv.ChannelID = cc.ChannelID.String()
	}
	if cc.DmGroupID != nil {
		inv.DmGroupID = *cc.DmGroupID
	}
	if cc.ThreadTs != nil {
		inv.ThreadTs = *cc.ThreadTs
	}
	if err := saveInvocation(ctx, token, inv); err != nil {
		logErr(ctx, "newResponseURL/save", err)
		return ""
	}
	return strings.TrimRight(responseBase(), "/") + "/command-responses/" + token
}

// Respond takes an app's answer at one of its response URLs.
func Respond(ctx context.Context, token string, body RespondBody) error {
	if len(token) != 48 {
		return ErrResponseGone
	}
	inv, ok, err := takeUse(ctx, token)
	if err != nil {
		return err
	}
	if !ok {
		return ErrResponseGone
	}
	card, _ := cardsBusiness.Reduce(body.Blocks)
	text := strings.TrimSpace(body.Text)
	if text == "" && len(card.Blocks) == 0 {
		return fmt.Errorf("an answer needs text or blocks")
	}
	if body.ResponseType == "in_channel" {
		if inv.ChannelID == "" {
			return ErrNoPlace
		}
		// The card is drawn as the post's body (the one rendering, which an
		// incoming webhook's blocks get too); the text is its fallback.
		if len(card.Blocks) > 0 {
			if body := cardsBusiness.HTML(card); body != "" {
				text = body
			}
		}
		return postInChannel(ctx, inv, text)
	}
	resp := &commandAdapter.CommandResponse{
		ResponseType:    "ephemeral",
		Ephemeral:       true,
		Text:            text,
		Blocks:          blocksOf(card),
		TriggerID:       inv.TriggerID,
		ReplaceOriginal: body.ReplaceOriginal,
	}
	sealAnswer(resp, appOwner(inv.AppID), inv.UserUUID)
	deliverEphemeral(ctx, inv.UserUUID, resp)
	return nil
}

// sealAnswer narrows a command's answer to the card subset and, when it has
// something to press, seals it for the one person it's shown to: a press goes
// to POST /cards/act and reaches its owner (an app, or a built-in's
// InteractionHandler) only after the server checks it.
func sealAnswer(resp *commandAdapter.CommandResponse, owner cardsBusiness.Owner, userUUID string) {
	if resp == nil || len(resp.Blocks) == 0 {
		return
	}
	raw, _ := json.Marshal(resp.Blocks)
	card, _ := cardsBusiness.Reduce(raw)
	resp.Blocks = blocksOf(card)
	if len(card.Actions()) == 0 || resp.TriggerID == "" {
		return
	}
	ref := cardsBusiness.CardRef{Host: cardsBusiness.HostEphemeral, ID: resp.TriggerID}
	if tok, err := cardsBusiness.Seal(ref, owner, card, []string{userUUID}, cardLife); err == nil {
		resp.CardToken = tok
	}
}

// appOwner is an app as the owner of the cards it answers with.
func appOwner(appID string) cardsBusiness.Owner {
	return cardsBusiness.Owner{Kind: cardsBusiness.OwnerApp, ID: appID}
}

// blocksOf is a card as the command answer's blocks (the same JSON).
func blocksOf(card cardsBusiness.Card) []commandAdapter.Block {
	if len(card.Blocks) == 0 {
		return nil
	}
	raw, _ := json.Marshal(card.Blocks)
	var out []commandAdapter.Block
	_ = json.Unmarshal(raw, &out)
	return out
}
