package monday

// GraphQL transport for monday.com API v2.
//
// One POST endpoint (https://api.monday.com/v2), the personal API token
// raw in the Authorization header (no "Bearer"), and a pinned
// API-Version header so a monday default-version bump can't silently
// change field shapes under a running import.
//
// monday reports limits in three shapes, and all of them land here:
//
//  1. HTTP 429 with {"errors":[{"message":…,"extensions":{"code":
//     "COMPLEXITY_BUDGET_EXHAUSTED"|"RATE_LIMIT_EXCEEDED"|…,
//     "retry_in_seconds":N}}]} — the 2025-01+ shape.
//  2. HTTP 200 with the same errors array (older versions, and some
//     concurrency errors).
//  3. The legacy top-level {"error_code":"ComplexityException",
//     "error_message":"… reset in 13 seconds","status_code":429}.
//
// A short wait (≤ maxInlineWait) is absorbed here: the transport sleeps
// and retries, because a per-minute complexity reset is routine during
// a crawl and failing Plan over it would be silly. A long one (the
// daily call cap) surfaces as *ErrRateLimited so the orchestrator's
// chunk worker parks the chunk without burning an attempt.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// graphqlEndpoint is monday's single GraphQL endpoint. A var so tests
// can point it at an httptest server; production never reassigns it.
var graphqlEndpoint = "https://api.monday.com/v2"

// apiVersion pins the schema. 2026-07 is the current stable version as
// of Oct 2026; bump deliberately (and re-run the transport tests) when
// monday retires it.
const apiVersion = "2026-07"

const (
	// maxRateRetries bounds how many limit errors one call absorbs
	// before giving up and surfacing ErrRateLimited.
	maxRateRetries = 5
	// maxInlineWait is the longest single wait the transport sleeps
	// through itself. Complexity budgets reset each minute.
	maxInlineWait = 65 * time.Second
	// defaultRateWait is used when monday gives no retry hint.
	defaultRateWait = 10 * time.Second
	// maxServerRetries covers transient 5xx from monday's edge.
	maxServerRetries = 2
)

// sleepFn is swapped by tests so retries don't actually wait.
var sleepFn = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// errAuth is returned for a token monday rejects outright.
var errAuth error = &importProvider.TokenRejected{Msg: "monday.com rejected the API token; open monday.com → your avatar → Developers → My access tokens, copy a fresh personal API token, and reconnect"}

// errServer marks a retryable 5xx.
type errServer struct {
	status int
	body   string
}

func (e *errServer) Error() string {
	return fmt.Sprintf("monday.com http %d: %s", e.status, e.body)
}

type gqlError struct {
	Message    string         `json:"message"`
	Extensions map[string]any `json:"extensions"`
}

// envelope covers both the GraphQL {data, errors} shape and monday's
// legacy top-level {error_code, error_message, status_code}.
type envelope struct {
	Data         json.RawMessage `json:"data"`
	Errors       json.RawMessage `json:"errors"`
	ErrorCode    string          `json:"error_code"`
	ErrorMessage string          `json:"error_message"`
	StatusCode   int             `json:"status_code"`
}

// gql runs one GraphQL query, absorbing short rate/complexity waits and
// transient 5xx, and decodes `data` into out.
func (p *Provider) gql(ctx context.Context, tok, query string, vars map[string]any, out any) error {
	rateTries, serverTries := 0, 0
	for {
		err := p.gqlOnce(ctx, tok, query, vars, out)
		if err == nil {
			return nil
		}
		var rl *importProvider.ErrRateLimited
		if errors.As(err, &rl) {
			wait := rl.RetryAfter
			if wait <= 0 {
				wait = defaultRateWait
			}
			if rateTries >= maxRateRetries || wait > maxInlineWait {
				return err
			}
			rateTries++
			if serr := sleepFn(ctx, wait); serr != nil {
				return serr
			}
			continue
		}
		var se *errServer
		if errors.As(err, &se) && serverTries < maxServerRetries {
			serverTries++
			if serr := sleepFn(ctx, time.Duration(serverTries)*2*time.Second); serr != nil {
				return serr
			}
			continue
		}
		return err
	}
}

func (p *Provider) gqlOnce(ctx context.Context, tok, query string, vars map[string]any, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.rl.Wait(ctx); err != nil {
		return err
	}
	if vars == nil {
		vars = map[string]any{}
	}
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, graphqlEndpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	// monday wants the raw token. Tolerate a pasted "Bearer " prefix.
	req.Header.Set("Authorization", strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(tok), "Bearer ")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("API-Version", apiVersion)

	resp, err := importProvider.SharedAPIClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Bodies are bounded: a page of 50 items with updates is well under
	// this, and a hostile upstream can't exhaust memory.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("monday.com read: %w", err)
	}

	var env envelope
	decodeErr := json.Unmarshal(raw, &env)
	errs := decodeErrors(env.Errors)

	// Limits first: they can arrive on 429 or on 200.
	if rl := rateLimitFrom(resp, env, errs); rl != nil {
		return rl
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return errAuth
	case resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("monday.com refused the request (403): %s", firstMessage(env, errs, raw))
	case resp.StatusCode >= 500:
		return &errServer{status: resp.StatusCode, body: snippet(raw)}
	case resp.StatusCode >= 400:
		return fmt.Errorf("monday.com http %d: %s", resp.StatusCode, firstMessage(env, errs, raw))
	}
	if decodeErr != nil {
		return fmt.Errorf("monday.com decode: %w", decodeErr)
	}

	for _, e := range errs {
		if isAuthCode(codeOf(e)) {
			return errAuth
		}
	}
	if env.ErrorCode != "" || env.ErrorMessage != "" {
		if isAuthCode(env.ErrorCode) {
			return errAuth
		}
		return fmt.Errorf("monday.com: %s", firstMessage(env, errs, raw))
	}

	hasData := len(env.Data) > 0 && string(env.Data) != "null"
	if len(errs) > 0 && !hasData {
		return fmt.Errorf("monday.com graphql: %s", errs[0].Message)
	}
	// Errors alongside data are monday's partial-data responses (an
	// asset that's gone, a column the token can't read). Keep the data.
	if !hasData {
		return errors.New("monday.com returned an empty data envelope")
	}
	return json.Unmarshal(env.Data, out)
}

// decodeErrors accepts the standard [{message, extensions}] shape and
// the occasional bare-string list.
func decodeErrors(raw json.RawMessage) []gqlError {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var errs []gqlError
	if err := json.Unmarshal(raw, &errs); err == nil {
		return errs
	}
	var strs []string
	if err := json.Unmarshal(raw, &strs); err == nil {
		out := make([]gqlError, 0, len(strs))
		for _, s := range strs {
			out = append(out, gqlError{Message: s})
		}
		return out
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil && one != "" {
		return []gqlError{{Message: one}}
	}
	return nil
}

func codeOf(e gqlError) string {
	if e.Extensions == nil {
		return ""
	}
	if c, ok := e.Extensions["code"].(string); ok {
		return c
	}
	return ""
}

// normCode upper-cases and drops separators so COMPLEXITY_BUDGET_EXHAUSTED,
// ComplexityBudgetExhausted and "Rate Limit Exceeded" compare equal.
func normCode(c string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(c) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

var rateCodes = map[string]bool{
	"COMPLEXITYBUDGETEXHAUSTED":    true,
	"COMPLEXITYEXCEPTION":          true,
	"RATELIMITEXCEEDED":            true,
	"MAXCONCURRENCYEXCEEDED":       true,
	"CONCURRENCYLIMITEXCEEDED":     true,
	"IPRATELIMITEXCEEDED":          true,
	"DAILYLIMITEXCEEDED":           true,
	"FIELDMINUTERATELIMITEXCEEDED": true,
	"MINUTELIMITEXCEEDED":          true,
	"MINUTELIMITRATEEXCEEDED":      true,
}

func isRateCode(c string) bool { return rateCodes[normCode(c)] }

func isRateMessage(m string) bool {
	m = strings.ToLower(m)
	return strings.Contains(m, "complexity budget exhausted") ||
		strings.Contains(m, "rate limit") ||
		strings.Contains(m, "minute limit") ||
		strings.Contains(m, "concurrency limit") ||
		strings.Contains(m, "daily limit")
}

var authCodes = map[string]bool{
	"UNAUTHORIZED":        true,
	"NOTAUTHENTICATED":    true,
	"INVALIDTOKEN":        true,
	"AUTHENTICATIONERROR": true,
}

func isAuthCode(c string) bool { return authCodes[normCode(c)] }

var resetInRe = regexp.MustCompile(`(?i)(?:reset|retry)\s+in\s+(\d+)\s*sec`)

// rateLimitFrom returns *ErrRateLimited when the response is any of
// monday's limit shapes, nil otherwise.
func rateLimitFrom(resp *http.Response, env envelope, errs []gqlError) *importProvider.ErrRateLimited {
	hit := false
	reason := ""
	var retry time.Duration
	for _, e := range errs {
		code := codeOf(e)
		if isRateCode(code) || isRateMessage(e.Message) {
			hit = true
			reason = code
			if reason == "" {
				reason = e.Message
			}
			if v, ok := e.Extensions["retry_in_seconds"]; ok {
				retry = secondsFrom(v)
			}
			if retry == 0 {
				retry = secondsFromMessage(e.Message)
			}
			break
		}
	}
	if !hit && (isRateCode(env.ErrorCode) || isRateMessage(env.ErrorMessage)) {
		hit = true
		reason = env.ErrorCode
		retry = secondsFromMessage(env.ErrorMessage)
	}
	if !hit && resp.StatusCode == http.StatusTooManyRequests {
		hit = true
		reason = "http 429"
	}
	if !hit {
		return nil
	}
	if retry == 0 {
		if s := parseRetryAfter(resp.Header.Get("Retry-After")); s > 0 {
			retry = time.Duration(s) * time.Second
		}
	}
	if normCode(reason) == "DAILYLIMITEXCEEDED" {
		reason = "daily API limit for this plan is used up"
	}
	return &importProvider.ErrRateLimited{RetryAfter: retry, Reason: "monday.com " + reason}
}

func secondsFrom(v any) time.Duration {
	switch n := v.(type) {
	case float64:
		if n > 0 {
			return time.Duration(n * float64(time.Second))
		}
	case string:
		if f, err := strconv.ParseFloat(n, 64); err == nil && f > 0 {
			return time.Duration(f * float64(time.Second))
		}
	}
	return 0
}

func secondsFromMessage(m string) time.Duration {
	if mm := resetInRe.FindStringSubmatch(m); len(mm) == 2 {
		if s, err := strconv.Atoi(mm[1]); err == nil && s > 0 {
			return time.Duration(s) * time.Second
		}
	}
	return 0
}

// parseRetryAfter handles numeric seconds and the HTTP-date form.
func parseRetryAfter(h string) int64 {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(h, 10, 64); err == nil && secs > 0 {
		return secs
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return int64(d.Seconds()) + 1
		}
	}
	return 0
}

func firstMessage(env envelope, errs []gqlError, raw []byte) string {
	if len(errs) > 0 && errs[0].Message != "" {
		return errs[0].Message
	}
	if env.ErrorMessage != "" {
		return env.ErrorMessage
	}
	return snippet(raw)
}

func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 512 {
		s = s[:512]
	}
	return s
}
