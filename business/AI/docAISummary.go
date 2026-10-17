package business

// Summarising a whole doc that is longer than the model can read at once
// (docs programme B9, ai-search-io P0-4): map, then reduce.
//
// The doc is cut at its headings into parts the model can read (a part too long
// on its own is cut again at blank lines), each part is summarised on its own
// with its section names kept, and the summary the person sees is written from
// those. Every section is read, so a fact from the last one can be in the
// summary; cutting the doc to the window kept only its start.

import (
	"context"
	"fmt"
	"strings"

	ai "github.com/akashc777/OneCamp/services/AI"
)

// summaryMaxParts bounds the map step: a local model takes seconds per part.
const summaryMaxParts = 24

// SplitDocForSummary cuts Markdown into parts of at most maxTokens each (as
// limits counts them), at headings first and then at blank lines. Pure.
func SplitDocForSummary(md string, limits ai.ModelLimits, maxTokens int) []string {
	md = strings.TrimSpace(md)
	if md == "" {
		return nil
	}
	if maxTokens < 64 {
		maxTokens = 64
	}
	var sections []string
	var cur []string
	inFence := false
	flush := func() {
		if s := strings.TrimSpace(strings.Join(cur, "\n")); s != "" {
			sections = append(sections, s)
		}
		cur = nil
	}
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
		}
		if !inFence && strings.HasPrefix(line, "#") && len(cur) > 0 {
			flush()
		}
		cur = append(cur, line)
	}
	flush()

	var pieces []string
	for _, s := range sections {
		if limits.EstimateTokens(s) <= maxTokens {
			pieces = append(pieces, s)
			continue
		}
		pieces = append(pieces, splitByParagraphs(s, limits, maxTokens)...)
	}
	return packPieces(pieces, limits, maxTokens)
}

// packPieces joins neighbouring pieces while they fit maxTokens together.
func packPieces(pieces []string, limits ai.ModelLimits, maxTokens int) []string {
	var parts []string
	var b strings.Builder
	for _, p := range pieces {
		if b.Len() > 0 && limits.EstimateTokens(b.String()+"\n\n"+p) > maxTokens {
			parts = append(parts, b.String())
			b.Reset()
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(p)
	}
	if b.Len() > 0 {
		parts = append(parts, b.String())
	}
	return parts
}

func splitByParagraphs(s string, limits ai.ModelLimits, maxTokens int) []string {
	var out []string
	var b strings.Builder
	for _, para := range strings.Split(s, "\n\n") {
		if b.Len() > 0 && limits.EstimateTokens(b.String()+"\n\n"+para) > maxTokens {
			out = append(out, b.String())
			b.Reset()
		}
		if limits.EstimateTokens(para) > maxTokens {
			// One paragraph longer than a part: split it at its sentences, so
			// nothing in it goes unread.
			if b.Len() > 0 {
				out = append(out, b.String())
				b.Reset()
			}
			out = append(out, splitLongParagraph(para, limits, maxTokens)...)
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(para)
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

// splitLongParagraph cuts a paragraph longer than a part into parts at the ends
// of its sentences, and a sentence longer than a part by its length. Every word
// is in one of the parts. Pure.
func splitLongParagraph(para string, limits ai.ModelLimits, maxTokens int) []string {
	maxRunes := int(float64(maxTokens) * limits.CharsPerToken())
	if maxRunes < 200 {
		maxRunes = 200
	}
	var out []string
	var b strings.Builder
	flush := func() {
		if t := strings.TrimSpace(b.String()); t != "" {
			out = append(out, t)
		}
		b.Reset()
	}
	for _, sentence := range sentencesOf(para) {
		for r := []rune(sentence); len(r) > maxRunes; r = []rune(sentence) {
			flush()
			out = append(out, string(r[:maxRunes]))
			sentence = string(r[maxRunes:])
		}
		if b.Len() > 0 && limits.EstimateTokens(b.String()+" "+sentence) > maxTokens {
			flush()
		}
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString(strings.TrimSpace(sentence))
	}
	flush()
	return out
}

// sentencesOf splits text after each ".", "!" or "?" that a space or a line
// break follows, and at line breaks. Pure.
func sentencesOf(text string) []string {
	var out []string
	r := []rune(text)
	start := 0
	for i := 0; i < len(r); i++ {
		end := r[i] == '\n' || ((r[i] == '.' || r[i] == '!' || r[i] == '?') && (i+1 == len(r) || r[i+1] == ' ' || r[i+1] == '\n'))
		if !end {
			continue
		}
		if t := strings.TrimSpace(string(r[start : i+1])); t != "" {
			out = append(out, t)
		}
		start = i + 1
	}
	if t := strings.TrimSpace(string(r[start:])); t != "" {
		out = append(out, t)
	}
	return out
}

// chatForSummary is one model call of the map step: a seam for tests.
var chatForSummary = func(ctx context.Context, llm ai.LLMProvider, messages []ai.ChatMessage, opts ai.ChatOptions) (string, error) {
	return ai.ChatWithRescue(ctx, llm, messages, opts)
}

const summaryPartPrompt = "You are summarising one part of a longer document. List its key points, decisions, names, dates and numbers as short bullets, under the part's own section headings. " + docAnswerRules

// SummariseDocParts is the map step: each part summarised on its own, in
// order, then, while the summaries together are still more than budget tokens,
// neighbouring summaries summarised together, for at most three more rounds, so
// the summary written from them reads every part. progress is told after each
// model call. The answer is the summaries, each headed "Part N:".
func SummariseDocParts(ctx context.Context, llm ai.LLMProvider, title string, parts []string, limits ai.ModelLimits, budget int, progress func(done, of int)) (string, error) {
	summaries, err := summariseEach(ctx, llm, title, parts, progress)
	if err != nil {
		return "", err
	}
	joined := joinPartSummaries(summaries)
	for round := 0; round < 3 && len(summaries) > 1 && limits.EstimateTokens(joined) > budget; round++ {
		groups := packPieces(summaries, limits, budget*4/5)
		if len(groups) >= len(summaries) {
			break
		}
		if summaries, err = summariseEach(ctx, llm, title, groups, progress); err != nil {
			return "", err
		}
		joined = joinPartSummaries(summaries)
	}
	return joined, nil
}

func summariseEach(ctx context.Context, llm ai.LLMProvider, title string, parts []string, progress func(done, of int)) ([]string, error) {
	out := make([]string, 0, len(parts))
	for i, p := range parts {
		user := fmt.Sprintf("Part %d of %d", i+1, len(parts))
		if title != "" {
			user += " of the document \"" + title + "\""
		}
		user += ":\n" + p
		answer, err := chatForSummary(ctx, llm, []ai.ChatMessage{
			{Role: "system", Content: summaryPartPrompt},
			{Role: "user", Content: user},
		}, ai.ChatOptions{Temperature: 0.2, MaxTokens: 400})
		if err != nil {
			return nil, err
		}
		out = append(out, CleanDocAnswer(answer, p))
		if progress != nil {
			progress(i+1, len(parts))
		}
	}
	return out, nil
}

func joinPartSummaries(summaries []string) string {
	var b strings.Builder
	for i, s := range summaries {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "Part %d:\n%s", i+1, s)
	}
	return b.String()
}

// PlanDocSummary decides how a doc's summary is read: in one go when the doc
// fits budget tokens, else the parts for the map step, each one the model can
// read whole. A doc of more than summaryMaxParts parts is summarised from its
// first summaryMaxParts, and cut says so. Pure.
func PlanDocSummary(md string, limits ai.ModelLimits, budget int) (parts []string, mapped, cut bool) {
	if limits.EstimateTokens(md) <= budget {
		return nil, false, false
	}
	parts = SplitDocForSummary(md, limits, budget*4/5)
	if len(parts) > summaryMaxParts {
		return parts[:summaryMaxParts], true, true
	}
	return parts, true, false
}
