package business

// Board AI - high-fidelity UI design mode. Instead of low-fidelity Excalidraw
// shapes, this turns a prompt into a complete, modern UI screen as Tailwind-
// styled HTML. The client renders it in a sandboxed iframe (a clean, Notion-
// like design studio) and can export it as PNG/SVG/HTML or import it into Figma
// or Canva. Producing HTML (vs canvas primitives) is what makes "production-
// grade, exportable" fidelity possible: the model is excellent at HTML/Tailwind,
// and HTML maps cleanly to SVG/PNG and to Figma/Canva import paths.

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	boardUIMaxPromptLen = 2000
	boardUIMaxHTMLBytes = 256 * 1024 // generous cap on generated markup
)

// GenerateBoardUIHTML turns a prompt into a single UI screen as Tailwind-styled
// HTML body markup. The result is sanitized (scripts, event handlers, iframes
// and javascript: URLs stripped) as defense in depth on top of the client's
// sandboxed, script-disabled iframe.
func GenerateBoardUIHTML(ctx context.Context, userUUID, prompt, device string) (html string, resolvedDevice string, err error) {
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return "", "", fmt.Errorf("AI is not enabled for this workspace")
	}

	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "", "", fmt.Errorf("a prompt is required")
	}
	if len(prompt) > boardUIMaxPromptLen {
		prompt = prompt[:boardUIMaxPromptLen]
	}

	resolvedDevice = strings.ToLower(strings.TrimSpace(device))
	if resolvedDevice != "desktop" {
		resolvedDevice = "mobile"
	}

	if err = svc.Resiliency.CheckRateLimit(ctx, userUUID); err != nil {
		return "", "", err
	}
	llm, cb := svc.ResolveUserModel(ctx, userUUID)
	if llm == nil {
		return "", "", fmt.Errorf("AI is not enabled for this workspace")
	}
	if err = cb.Allow(); err != nil {
		return "", "", err
	}

	messages := []ai.ChatMessage{
		{Role: "system", Content: boardUIHTMLSystemPrompt(resolvedDevice)},
		{Role: "user", Content: prompt},
	}
	opts := ai.ChatOptions{Temperature: 0.4, MaxTokens: 4096, LowLatency: true}

	out, err := ai.ChatWithRescue(ctx, llm, messages, opts)
	if err != nil {
		cb.RecordResult(err)
		helpers.LogErrorWithContext(ctx, "business/GenerateBoardUIHTML LLM failed: %+v", err)
		return "", "", fmt.Errorf("the AI could not generate a design right now")
	}
	cb.RecordSuccess()

	html = sanitizeGeneratedHTML(extractHTMLBody(out))
	if strings.TrimSpace(html) == "" {
		// One corrective retry before surfacing an error.
		retry := []ai.ChatMessage{
			{Role: "system", Content: boardUIHTMLSystemPrompt(resolvedDevice)},
			{Role: "user", Content: prompt},
			{Role: "assistant", Content: out},
			{Role: "user", Content: "Return ONLY the HTML markup for the screen, styled with Tailwind classes. No code fences, no commentary."},
		}
		if retryOut, rerr := ai.ChatWithRescue(ctx, llm, retry, opts); rerr == nil {
			html = sanitizeGeneratedHTML(extractHTMLBody(retryOut))
		}
	}
	if strings.TrimSpace(html) == "" {
		helpers.LogErrorWithContext(ctx, "business/GenerateBoardUIHTML empty markup, raw=%q", truncateForLog(out))
		return "", "", fmt.Errorf("the AI returned an unexpected response, please try again")
	}
	if len(html) > boardUIMaxHTMLBytes {
		html = html[:boardUIMaxHTMLBytes]
	}
	return html, resolvedDevice, nil
}

const boardUIMaxRefineInputBytes = 64 * 1024 // cap on HTML we feed back for refinement

// RefineBoardUIHTML takes an existing generated screen and an instruction, and
// returns a corrected/improved version. It is used for two things: applying a
// user-described change ("make the header sticky", "use a green accent"), and
// AI self-QA (fixing overflow, overlap, clipping, misalignment, low contrast
// and broken structure). It reuses the same sanitization and extraction as
// generation so the output is always safe to render in the sandboxed iframe.
func RefineBoardUIHTML(ctx context.Context, userUUID, currentHTML, instruction, device string) (html string, resolvedDevice string, err error) {
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return "", "", fmt.Errorf("AI is not enabled for this workspace")
	}

	currentHTML = strings.TrimSpace(currentHTML)
	if currentHTML == "" {
		return "", "", fmt.Errorf("there is no design to refine yet")
	}
	if len(currentHTML) > boardUIMaxRefineInputBytes {
		currentHTML = currentHTML[:boardUIMaxRefineInputBytes]
	}

	instruction = strings.TrimSpace(instruction)
	if instruction == "" {
		// Default to an autonomous design-QA pass.
		instruction = "Review this screen for visual bugs and fix them: content that overflows or is clipped by its container, overlapping or misaligned elements, inconsistent spacing, low-contrast or unreadable text, broken or unbalanced layout, and any element that runs outside the device frame. Keep the design intent and content the same; only correct the issues and tighten the polish."
	}
	if len(instruction) > boardUIMaxPromptLen {
		instruction = instruction[:boardUIMaxPromptLen]
	}

	resolvedDevice = strings.ToLower(strings.TrimSpace(device))
	if resolvedDevice != "desktop" {
		resolvedDevice = "mobile"
	}

	if err = svc.Resiliency.CheckRateLimit(ctx, userUUID); err != nil {
		return "", "", err
	}
	llm, cb := svc.ResolveUserModel(ctx, userUUID)
	if llm == nil {
		return "", "", fmt.Errorf("AI is not enabled for this workspace")
	}
	if err = cb.Allow(); err != nil {
		return "", "", err
	}

	messages := []ai.ChatMessage{
		{Role: "system", Content: boardUIRefineSystemPrompt(resolvedDevice)},
		{Role: "user", Content: "CURRENT SCREEN MARKUP:\n" + currentHTML + "\n\nINSTRUCTION:\n" + instruction},
	}
	opts := ai.ChatOptions{Temperature: 0.3, MaxTokens: 4096, LowLatency: true}

	out, err := ai.ChatWithRescue(ctx, llm, messages, opts)
	if err != nil {
		cb.RecordResult(err)
		helpers.LogErrorWithContext(ctx, "business/RefineBoardUIHTML LLM failed: %+v", err)
		return "", "", fmt.Errorf("the AI could not refine the design right now")
	}
	cb.RecordSuccess()

	html = sanitizeGeneratedHTML(extractHTMLBody(out))
	if strings.TrimSpace(html) == "" {
		helpers.LogErrorWithContext(ctx, "business/RefineBoardUIHTML empty markup, raw=%q", truncateForLog(out))
		return "", "", fmt.Errorf("the AI returned an unexpected response, please try again")
	}
	if len(html) > boardUIMaxHTMLBytes {
		html = html[:boardUIMaxHTMLBytes]
	}
	return html, resolvedDevice, nil
}

func boardUIRefineSystemPrompt(device string) string {
	frame := "a mobile app screen, exactly 390px wide and at least 844px tall (portrait)"
	if device == "desktop" {
		frame = "a desktop web app screen, 1280px wide"
	}
	return strings.Join([]string{
		"You are a senior UI engineer doing design QA on " + frame + ". You receive an existing screen as HTML body markup and an instruction.",
		"Fix any visual defects (overflow, clipping, overlap, misalignment, inconsistent spacing, low contrast, unreadable text, broken or unbalanced structure, elements escaping the device frame) AND apply the instruction.",
		"Preserve the original design intent, layout and content unless the instruction says otherwise. Improve, do not redesign from scratch.",
		"",
		"TECHNICAL RULES (MUST follow exactly):",
		"- Output ONLY the complete corrected HTML markup for the screen. No <html>/<head>/<body>, no markdown, no code fences, no commentary.",
		"- Style EVERYTHING with Tailwind utility classes (the host already loads Tailwind and Inter). Do NOT use <style> blocks or inline style attributes, except inline style is allowed ONLY for a CSS gradient background on a placeholder element.",
		"- Never use pure black text; use the gray scale. Keep a consistent 4/8px spacing rhythm, soft shadows, hairline borders and rounded corners.",
		"- Icons: inline SVG only, 24x24, stroke='currentColor', stroke-width='1.5'. No emoji as icons, no external image URLs.",
		"- Do NOT include <script>, event-handler attributes, <iframe>, or any external resource URLs.",
		"- Wrap the whole screen in ONE root <div> sized exactly for the device with an app-like background.",
	}, "\n")
}

func boardUIHTMLSystemPrompt(device string) string {
	frame := "a mobile app screen, exactly 390px wide and at least 844px tall (portrait). Begin with a realistic iOS-style status bar (time on the left; signal, wifi and battery glyphs on the right) and, where it fits the app, a bottom tab bar with 4-5 items and a clear active state."
	if device == "desktop" {
		frame = "a desktop web app screen, 1280px wide. Use a top navigation bar and, where appropriate, a left sidebar plus a main content area with a real grid or multi-column layout."
	}
	return strings.Join([]string{
		"You are a world-class senior product designer (ex-Stripe, Linear, Vercel). Design " + frame,
		"Return ONE complete, pixel-polished, production-grade UI screen as HTML. The bar is Dribbble/Figma top-shot quality, not a wireframe.",
		"",
		"VISUAL SYSTEM (apply consistently):",
		"- Pick a single tasteful accent color appropriate to the product and use it deliberately (primary actions, active states, highlights). Neutrals: use the gray scale (text gray-900/700/500, borders gray-100/200, surfaces white and gray-50). Never use pure black text.",
		"- Spacing on a consistent 4/8px rhythm. Generous padding. Group related content; separate sections with whitespace, not heavy lines.",
		"- Depth: soft shadows (shadow-sm, shadow-md, hover:shadow-lg), 1px hairline borders (border border-gray-100), rounded-2xl cards and rounded-xl controls. Use subtle gradients for hero areas, banners, and avatars.",
		"- Typography hierarchy: clear sizes/weights (e.g. text-2xl font-semibold tracking-tight for titles, text-sm text-gray-500 for secondary). The host sets Inter as the font.",
		"",
		"COMPONENT QUALITY:",
		"- Real, composed components: app bar with title + leading/trailing icon buttons; cards with media (use tasteful gradient blocks as image placeholders), title, subtitle and actions; list rows with a leading icon/avatar, two lines of text and a trailing chevron or value; primary (filled accent) and secondary (outline) buttons; inputs with labels and placeholders; badges/pills; segmented controls or tabs with an active state.",
		"- Icons: inline SVG only, 24x24, stroke='currentColor', stroke-width='1.5', rounded line caps. Consistent icon family. No emoji as icons.",
		"- Avatars: gradient-filled circles with initials, or solid color blocks. No external images.",
		"- Realistic, specific copy and data (names, prices, dates, metrics). Never lorem ipsum.",
		"",
		"TECHNICAL RULES (MUST follow exactly):",
		"- Output ONLY the HTML markup for the screen. No <html>/<head>/<body>, no markdown, no code fences, no commentary.",
		"- Style EVERYTHING with Tailwind utility classes (the host already loads Tailwind and Inter). Do NOT use <style> blocks or inline style attributes, except inline style is allowed ONLY for a CSS gradient background on a placeholder element.",
		"- Do NOT include <script>, event-handler attributes, <iframe>, or any external resource URLs (no <img src>).",
		"- Wrap the whole screen in ONE root <div> sized exactly for the device with an app-like background, so it renders cleanly inside a device frame.",
	}, "\n")
}

// extractHTMLBody strips code fences and, if the model returned a full document,
// returns the inner <body> markup.
func extractHTMLBody(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```html")
		s = strings.TrimPrefix(s, "```HTML")
		s = strings.TrimPrefix(s, "```")
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}
	lower := strings.ToLower(s)
	if bi := strings.Index(lower, "<body"); bi >= 0 {
		if gt := strings.Index(s[bi:], ">"); gt >= 0 {
			rest := s[bi+gt+1:]
			if be := strings.Index(strings.ToLower(rest), "</body>"); be >= 0 {
				return strings.TrimSpace(rest[:be])
			}
			return strings.TrimSpace(rest)
		}
	}
	return strings.TrimSpace(s)
}

var (
	reScriptBlock = regexp.MustCompile(`(?is)<script.*?</script>`)
	reScriptOpen  = regexp.MustCompile(`(?is)</?script[^>]*>`)
	reIframeBlock = regexp.MustCompile(`(?is)<iframe.*?</iframe>`)
	reIframeOpen  = regexp.MustCompile(`(?is)</?iframe[^>]*>`)
	reOnAttr      = regexp.MustCompile(`(?is)\son[a-z]+\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	reJsURL       = regexp.MustCompile(`(?is)(href|src)\s*=\s*("javascript:[^"]*"|'javascript:[^']*')`)
)

// sanitizeGeneratedHTML strips active content. This is defense in depth: the
// client renders the markup in an iframe with scripts disabled via sandbox, so
// even unsanitized script could not execute, but we remove it regardless.
func sanitizeGeneratedHTML(s string) string {
	s = reScriptBlock.ReplaceAllString(s, "")
	s = reScriptOpen.ReplaceAllString(s, "")
	s = reIframeBlock.ReplaceAllString(s, "")
	s = reIframeOpen.ReplaceAllString(s, "")
	s = reOnAttr.ReplaceAllString(s, "")
	s = reJsURL.ReplaceAllString(s, `$1="#"`)
	return strings.TrimSpace(s)
}
