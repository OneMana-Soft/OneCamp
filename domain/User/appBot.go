package domain

import (
	"context"

	"github.com/google/uuid"
)

// An installed app's own principal (platform P-API; design/platform/
// API_CONTRACT.md §3.1): the user an app's tokens act as and its messages are
// written by, so they read as the app's and notify like people's. Kept in its
// own file: botKind.go names the kinds, this one the app's.

// AppBotPrefix begins an app bot's username and email.
const AppBotPrefix = "app-bot-"

// BotKindApp is an installed app's principal: it runs no AI, and acts only
// where the app was added (business/App's audience), as a member at most.
const BotKindApp BotKind = "app"

// AppBotUsername is the app bot's unique handle, keyed on the app's id (two
// apps may share a name). Never change the format: it would orphan app bots
// already provisioned, and the messages they wrote.
func AppBotUsername(appID uuid.UUID) string { return AppBotPrefix + appID.String() }

// AppBotEmail keys the app bot's row.
func AppBotEmail(appID uuid.UUID) string { return AppBotUsername(appID) + BotEmailDomain }

// EnsureAppBotUser returns the app's bot principal, creating its row on first
// call and reconciling its display name (the app's) on later ones.
func EnsureAppBotUser(ctx context.Context, appID uuid.UUID, displayName string) (uuid.UUID, error) {
	if displayName == "" {
		displayName = "App"
	}
	return EnsureBotUser(ctx, AppBotEmail(appID), AppBotUsername(appID), displayName)
}
