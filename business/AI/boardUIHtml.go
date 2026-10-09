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
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
	"golang.org/x/net/html"
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

// sanitizeGeneratedHTML keeps a generated screen's design and drops whatever
// in it could act: script-like elements with their content, every on* handler,
// and any URL that isn't a web, mail, phone, relative or data-image one.
//
// The markup is a model's, so it's untrusted, and the preview frame runs
// scripts (it needs Tailwind's) with the app's origin (the PNG export reads
// it), so the frame's CSP is what stops inline script there. This is the
// layer under it, and what Copy and Download HTML hand out. It reads the
// markup with an HTML tokenizer: the patterns it replaces missed a handler
// written after a "/" or a closing quote (<img/onerror=...>,
// <img src="x"onerror=...>), and a scheme behind a character reference.
func sanitizeGeneratedHTML(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var b strings.Builder
	dropping := "" // the element whose content is being dropped
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken: // io.EOF, or input the tokenizer gave up on
			return strings.TrimSpace(b.String())
		case html.StartTagToken, html.SelfClosingTagToken, html.EndTagToken:
			tok := z.Token()
			name := strings.ToLower(tok.Data)
			if dropping != "" {
				if tt == html.EndTagToken && name == dropping {
					dropping = ""
				}
				continue
			}
			if droppedWithContent[name] {
				if tt == html.StartTagToken {
					dropping = name
				}
				continue
			}
			if droppedTag[name] {
				continue
			}
			if tt != html.EndTagToken {
				tok.Attr = safeAttrs(tok.Attr)
			}
			b.WriteString(tok.String())
		case html.TextToken:
			if dropping == "" {
				// Raw, not re-escaped: a <style> block's CSS must keep its ">"
				// selectors. A text token can't hold a tag; the tokenizer
				// ended it at the next one.
				b.Write(z.Raw())
			}
		}
		// Comments and doctypes are dropped.
	}
}

var (
	// droppedWithContent go with everything inside them.
	droppedWithContent = map[string]bool{
		"script": true, "iframe": true, "frame": true, "frameset": true,
		"object": true, "applet": true, "noembed": true, "noframes": true,
	}
	// droppedTag go alone: they have no content, or none worth dropping.
	droppedTag = map[string]bool{"embed": true, "base": true, "meta": true, "link": true}
	// urlAttrs hold a URL; it's kept only when safeDesignURL says so.
	urlAttrs = map[string]bool{
		"href": true, "src": true, "xlink:href": true, "action": true, "formaction": true,
		"poster": true, "background": true, "cite": true, "data": true, "longdesc": true,
	}
	// droppedAttrs carry markup or URLs a design has no use for.
	droppedAttrs = map[string]bool{"srcdoc": true, "srcset": true, "ping": true, "manifest": true, "codebase": true}
)

// safeAttrs is attrs without handlers, the attributes in droppedAttrs, and
// URLs safeDesignURL refuses. The tokenizer has already decoded character
// references in the values.
func safeAttrs(attrs []html.Attribute) []html.Attribute {
	out := attrs[:0]
	for _, a := range attrs {
		key := strings.ToLower(a.Key)
		if a.Namespace != "" {
			key = strings.ToLower(a.Namespace) + ":" + key
		}
		switch {
		case strings.HasPrefix(key, "on"), droppedAttrs[key]:
			continue
		case urlAttrs[key] && !safeDesignURL(key, a.Val):
			continue
		}
		out = append(out, a)
	}
	return out
}

// safeDesignURL reports whether a URL may stay in a design: http, https,
// mailto, tel, or no scheme at all (a relative path or a #fragment); and, for
// an image's src or poster, a data:image URL. The scheme is read with every
// control character and space taken out, as a browser drops them. Pure.
func safeDesignURL(attr, v string) bool {
	clean := strings.ToLower(strings.Map(func(r rune) rune {
		if r <= ' ' || r == 0x7f || r == 0xa0 || r == 0xad || r == 0xfeff || (r >= 0x2000 && r <= 0x200f) {
			return -1
		}
		return r
	}, v))
	colon := strings.IndexByte(clean, ':')
	if colon < 0 || strings.ContainsAny(clean[:colon], "/?#") {
		return true // no scheme
	}
	switch clean[:colon] {
	case "http", "https", "mailto", "tel":
		return true
	case "data":
		return (attr == "src" || attr == "poster") && strings.HasPrefix(clean, "data:image/")
	}
	return false
}
