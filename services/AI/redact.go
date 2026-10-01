package ai

// PII redaction before cloud egress (AI data residency, Requirement 3).
//
// When redaction is enabled AND the target model endpoint is non-local
// (cloud), every outbound text (system + user + context messages, summarize
// content, embedding inputs) is scrubbed of detected PII before it is
// serialized into the provider request. Local endpoints are never redacted
// (data stays on the customer's infrastructure, so redaction there would only
// degrade answers).
//
// Design properties:
//   - Linear, single-pass per pattern using RE2 (Go's regexp), which has NO
//     catastrophic backtracking, so it can never become a CPU/DoS vector.
//   - Fail closed: a text larger than maxRedactBytes is refused rather than
//     sent partially scanned, so PII can never slip past the scan window.
//   - Stable, type-labeled placeholders ([EMAIL_1], [CARD_2], …) so the same
//     value maps to the same placeholder within one request and the model can
//     still reason about "the same person/card" structurally.
//   - Counts only are surfaced for audit/metrics; raw values are never logged.

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
)

// maxRedactBytes bounds a single text we will scan. Legitimate prompts are
// already capped by the context-window budget upstream; anything past this is
// abnormal, so we refuse (fail closed) rather than risk leaking unscanned
// PII. RE2 is linear, so this bound is generous.
const maxRedactBytes = 8 << 20 // 8 MiB

// errRedactOversize is returned when content exceeds maxRedactBytes; the
// caller refuses the cloud request rather than transmit unscanned content.
var errRedactOversize = errors.New("ai: content exceeds the PII-redaction size limit; refusing to send it to a cloud model")

// piiPattern is one compiled detector.
type piiPattern struct {
	label string
	re    *regexp.Regexp
	luhn  bool // when true, only redact matches that pass the Luhn checksum (cards)
}

// Redactor holds the compiled default + admin-custom detectors. Built once per
// provider client (at construction) so there is no per-request compile cost.
// A nil *Redactor is a valid no-op (used for local endpoints / redaction off).
type Redactor struct {
	patterns []piiPattern
}

// defaultPIIPatterns are the built-in detectors. Order matters: more specific /
// checksum-validated patterns run before looser ones (card before phone) so a
// 16-digit card is not partially eaten by the phone matcher. Each replaced span
// is invisible to later patterns.
func defaultPIIPatterns() []piiPattern {
	return []piiPattern{
		{label: "EMAIL", re: regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)},
		// IBAN: 2-letter country + 2 check digits + 11-30 alphanumerics.
		{label: "IBAN", re: regexp.MustCompile(`\b[A-Z]{2}\d{2}[A-Z0-9]{11,30}\b`)},
		// Credit-card-shaped: 13-19 digits with optional space/dash grouping,
		// then Luhn-validated to cut false positives (order/invoice numbers).
		{label: "CARD", re: regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`), luhn: true},
		// US SSN-shaped government identifier.
		{label: "GOV_ID", re: regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)},
		// Phone: optional country code, common separators, 10-14 digits total.
		{label: "PHONE", re: regexp.MustCompile(`\+?\d{1,3}[\s.\-]?\(?\d{2,4}\)?[\s.\-]?\d{3,4}[\s.\-]?\d{3,4}`)},
	}
}

// NewRedactor compiles the default detectors plus any valid custom patterns.
// Invalid custom regexes are skipped (logged) so a bad admin pattern can never
// fail a request. Returns nil only if there are somehow zero usable patterns
// (impossible, since defaults always compile) — callers treat nil as no-op.
func NewRedactor(customPatterns []string) *Redactor {
	pats := defaultPIIPatterns()
	for i, raw := range customPatterns {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		re, err := regexp.Compile(p)
		if err != nil {
			helpers.MessageLogs.ErrorLog.Printf("AI redaction: skipping invalid custom PII pattern #%d: %v", i+1, err)
			continue
		}
		pats = append(pats, piiPattern{label: "CUSTOM", re: re})
	}
	return &Redactor{patterns: pats}
}

// Apply scrubs a single text, returning the redacted text and a per-type count
// of redactions. A nil Redactor or empty text is a no-op.
func (r *Redactor) Apply(text string) (string, map[string]int) {
	counts := map[string]int{}
	if r == nil || text == "" {
		return text, counts
	}
	// Stable placeholder per distinct raw value within this one call, so
	// repeated occurrences collapse to the same token.
	placeholders := map[string]string{}
	seq := map[string]int{}

	for _, p := range r.patterns {
		label := p.label
		text = p.re.ReplaceAllStringFunc(text, func(match string) string {
			if p.luhn && !luhnValid(match) {
				return match // not a real card number — leave it
			}
			key := label + "\x00" + match
			if ph, ok := placeholders[key]; ok {
				return ph
			}
			seq[label]++
			ph := "[" + label + "_" + strconv.Itoa(seq[label]) + "]"
			placeholders[key] = ph
			counts[label]++
			return ph
		})
	}
	return text, counts
}

// ApplyMessages redacts every message's content, preserving roles. It fails
// closed if any single message exceeds maxRedactBytes. Returns the redacted
// copy and the merged per-type counts.
func (r *Redactor) ApplyMessages(msgs []ChatMessage) ([]ChatMessage, map[string]int, error) {
	if r == nil {
		return msgs, nil, nil
	}
	total := map[string]int{}
	out := make([]ChatMessage, len(msgs))
	for i, m := range msgs {
		if len(m.Content) > maxRedactBytes {
			return nil, nil, errRedactOversize
		}
		red, c := r.Apply(m.Content)
		out[i] = ChatMessage{Role: m.Role, Content: red}
		for k, v := range c {
			total[k] += v
		}
	}
	return out, total, nil
}

// ApplyTexts redacts a slice of plain strings (embedding inputs), failing
// closed on oversize input.
func (r *Redactor) ApplyTexts(texts []string) ([]string, map[string]int, error) {
	if r == nil {
		return texts, nil, nil
	}
	total := map[string]int{}
	out := make([]string, len(texts))
	for i, t := range texts {
		if len(t) > maxRedactBytes {
			return nil, nil, errRedactOversize
		}
		red, c := r.Apply(t)
		out[i] = red
		for k, v := range c {
			total[k] += v
		}
	}
	return out, total, nil
}

// luhnValid reports whether the digits embedded in s pass the Luhn checksum
// and have a card-plausible length (13-19 digits).
func luhnValid(s string) bool {
	var digits []int
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits = append(digits, int(r-'0'))
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := digits[i]
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// logRedaction emits a structured, content-free record of what was redacted on
// an outbound cloud request (counts by type only — never the raw values). Used
// for the compliance-visibility requirement without flooding the audit table.
func logRedaction(ctx context.Context, provider string, counts map[string]int) {
	if len(counts) == 0 {
		return
	}
	parts := make([]string, 0, len(counts))
	for label, n := range counts {
		if n > 0 {
			parts = append(parts, label+"="+strconv.Itoa(n))
		}
	}
	if len(parts) == 0 {
		return
	}
	helpers.LogInfoWithContext(ctx, "AI PII redaction applied before cloud egress: provider=%s %s", provider, strings.Join(parts, " "))
}
