package business

import (
	"context"
	"fmt"
	"net/http"
	"time"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	"github.com/akashc777/OneCamp/helpers"
	slashModel "github.com/akashc777/OneCamp/models/postgres/SlashCommand"
	"github.com/google/uuid"
)

// testApp.go — the admin "Test" action. Verifies an installed app's stored
// credentials actually work, so an admin can confirm setup before relying on
// it (instead of discovering a bad key only when a user runs the command).
//
// The test is per-kind and does a REAL, side-effect-free check:
//   - Giphy (and any app with a known live probe): hit the provider's API with
//     the stored key and report success/failure with a human message.
//   - external apps with a handler_url: send a signed connectivity probe and
//     report whether the endpoint is reachable / returns 2xx.
//   - oauth apps: report whether a token is present (connected). A deeper live
//     call would need a provider-specific endpoint; "connected" is the honest,
//     generic signal.

// TestApp runs a credential/connectivity check for an installed app and returns
// a structured result. Never returns an error for an expected failure (bad
// key, unreachable handler) — those are reported in the result so the FE can
// show a clear message; it only errors for "app not found".
func TestApp(ctx context.Context, appID uuid.UUID) (*commandAdapter.AppTestResult, error) {
	app, err := slashModel.GetAppByID(ctx, appID)
	if err != nil {
		return nil, err
	}
	if app == nil {
		return nil, fmt.Errorf("app not found")
	}

	// App-specific live probes self-register by slug (best signal). This keeps
	// TestApp generic — no per-app switch to maintain.
	if probe, ok := appTestRegistry[app.Slug]; ok {
		return probe(ctx, app.Id), nil
	}

	// OAuth apps: connectivity == token present.
	if app.Kind == slashModel.AppKindOAuth {
		if appOAuthConnected(ctx, app.Id) {
			return ok("Connected. An access token is stored for this app."), nil
		}
		return fail("Not connected. Click Connect to authorize this app."), nil
	}

	// External apps: probe the handler URL with a signed, side-effect-free
	// connectivity check (Slack-style ssl_check).
	if app.Kind == slashModel.AppKindExternal {
		handler := ""
		if app.HandlerUrl != nil {
			handler = *app.HandlerUrl
		}
		if handler == "" {
			return fail("No handler URL configured for this app."), nil
		}
		return testHandlerURL(ctx, app.Id, handler), nil
	}

	return ok("No automated test for this app; it appears installed."), nil
}

// testHandlerURL sends a signed connectivity probe to an external app's
// handler. A 2xx (or any HTTP response at all) proves reachability; the app is
// expected to recognize the ssl_check payload and 200 it.
func testHandlerURL(ctx context.Context, appID uuid.UUID, handlerURL string) *commandAdapter.AppTestResult {
	if _, err := helpers.ValidateOutboundURL(handlerURL, false); err != nil {
		return fail(fmt.Sprintf("Handler URL rejected: %v", err))
	}

	cmd, _ := slashModel.GetAppByID(ctx, appID)
	signing := ""
	if cmd != nil && cmd.SigningSecret != nil {
		// Decrypt the signing secret for the probe signature.
		if dec, derr := helpers.DecryptSecret(*cmd.SigningSecret); derr == nil {
			signing = dec
		}
	}

	payload := map[string]interface{}{
		"type":      "ssl_check",
		"ssl_check": "1",
	}
	resp := probeSigned(ctx, handlerURL, signing, payload)
	if resp == nil {
		return fail("Handler URL is unreachable or timed out.")
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return ok(fmt.Sprintf("Reachable — handler responded %d.", resp.StatusCode))
	}
	return fail(fmt.Sprintf("Handler responded %d (expected 2xx).", resp.StatusCode))
}

// probeSigned is a lightweight HEAD-of-life POST that reuses the dispatch
// signing scheme but discards the body — we only care about reachability.
func probeSigned(ctx context.Context, url, secret string, payload map[string]interface{}) *http.Response {
	pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	// Reuse postSigned's transport but we only need the status; do a minimal
	// signed POST inline to avoid coupling to the response parser.
	return doSignedProbe(pctx, url, secret, payload)
}

// helpers to build results.
func ok(msg string) *commandAdapter.AppTestResult {
	return &commandAdapter.AppTestResult{Success: true, Message: msg}
}
func fail(msg string) *commandAdapter.AppTestResult {
	return &commandAdapter.AppTestResult{Success: false, Message: msg}
}
