package business

// AI social-post composer.
//
// Turns a topic (or pasted release notes / announcement) into platform-native
// drafts for X (single tweet + thread) and Reddit (title + body), each tuned
// to that platform's norms. The value over a generic "write a tweet" prompt is
// (1) multiple tailored variants in one pass, and (2) baked-in platform
// best-practices (length, tone, Reddit's anti-promo culture).
//
// These are DRAFTS to review/edit and post yourself. OneCamp does not auto-post
// to Reddit/X: those platforms restrict automated promotion (account-ban risk)
// and need per-account API/OAuth setup, so publishing stays a human action.

import (
	"context"
	"fmt"
	"strings"
	"time"

	ai "github.com/akashc777/OneCamp/services/AI"
)

// socialPlatform describes a target and how to write for it.
type socialPlatform struct {
	Key      string
	Label    string
	Guidance string
}

// supportedSocialPlatforms is the allowlist of targets the composer drafts for.
var supportedSocialPlatforms = map[string]socialPlatform{
	"x_tweet": {
		Key:   "x_tweet",
		Label: "X / Twitter (single post)",
		Guidance: "A single post, hard limit 280 characters. Punchy, concrete, lead with the value. " +
			"At most one hashtag, no hashtag spam. No clickbait. Plain text.",
	},
	"x_thread": {
		Key:   "x_thread",
		Label: "X / Twitter (thread)",
		Guidance: "A thread of 3-6 posts, each under 280 characters, numbered like 1/ 2/ 3/. " +
			"First post is a strong hook that stands alone. One idea per post. End with a soft call to action.",
	},
	"reddit": {
		Key:   "reddit",
		Label: "Reddit (title + body)",
		Guidance: "Give a TITLE line then a BODY in markdown. Reddit hates ads: write authentically and value-first, " +
			"share context/learnings, be honest if it is your own project, avoid marketing language and excessive links. " +
			"Conversational, no hashtags.",
	},
	"linkedin": {
		Key:   "linkedin",
		Label: "LinkedIn",
		Guidance: "A short professional post, a few one-to-two sentence paragraphs, concrete and humble. " +
			"At most a couple of hashtags at the end.",
	},
}

// defaultSocialPlatforms is used when the caller specifies none.
var defaultSocialPlatforms = []string{"x_tweet", "x_thread", "reddit"}

// SocialPost is one drafted variant.
type SocialPost struct {
	Platform string `json:"platform"`
	Label    string `json:"label"`
	Content  string `json:"content"`
}

const socialMaxTopicChars = 4000

// DraftSocialPosts drafts platform-tailored social copy for the given topic.
// platforms is a subset of the supported keys (empty = a sensible default).
// Rate-limited per user; runs on the user's resolved model.
func DraftSocialPosts(ctx context.Context, userUUID, topic string, platforms []string) ([]SocialPost, error) {
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return nil, fmt.Errorf("AI service is not enabled")
	}
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return nil, fmt.Errorf("describe what you want to post about")
	}
	if len(topic) > socialMaxTopicChars {
		topic = topic[:socialMaxTopicChars] + " ..."
	}

	// Resolve + validate requested platforms, preserving a stable order.
	selected := normalizeSocialPlatforms(platforms)
	if len(selected) == 0 {
		return nil, fmt.Errorf("no valid platforms selected")
	}

	llm, cb := svc.ResolveUserModel(ctx, userUUID)
	if err := cb.Allow(); err != nil {
		return nil, err
	}
	if err := svc.Resiliency.CheckRateLimit(ctx, userUUID); err != nil {
		return nil, err
	}

	var guide strings.Builder
	for _, key := range selected {
		p := supportedSocialPlatforms[key]
		guide.WriteString(fmt.Sprintf("- %s (key: %s): %s\n", p.Label, p.Key, p.Guidance))
	}

	messages := []ai.ChatMessage{
		{Role: "system", Content: socialSystemPrompt(guide.String())},
		{Role: "user", Content: fmt.Sprintf("Topic to post about:\n%s\n\nDraft one variant per requested platform.", topic)},
	}
	opts := ai.ChatOptions{Temperature: 0.7, MaxTokens: 1200, Seed: int(time.Now().UnixNano() % 1000000)}

	answer, err := ai.ChatWithRescue(ctx, llm, messages, opts)
	if err != nil {
		cb.RecordResult(err)
		return nil, fmt.Errorf("social post generation failed: %w", err)
	}
	cb.RecordSuccess()

	posts := parseSocialPosts(answer, selected)
	if len(posts) == 0 {
		// Model didn't follow the block format; return the whole text as one
		// draft so the user still gets something useful.
		posts = []SocialPost{{Platform: "draft", Label: "Draft", Content: strings.TrimSpace(SanitizeResponse(answer))}}
	}
	return posts, nil
}

func socialSystemPrompt(platformGuide string) string {
	return fmt.Sprintf(`You are a marketing copywriter drafting social media posts. Write genuinely good, platform-native copy that a real person would post, not generic hype.

Draft a variant for each of these platforms, following its rules:
%s
Output format — for EACH platform, emit a block delimited EXACTLY like this and nothing else around it:
@@@<key>@@@
<the post content>
@@@end@@@

Rules:
- Use the exact key shown in parentheses above.
- Ground the copy ONLY in the topic provided. Do not invent features, metrics, or quotes.
- No preamble or commentary outside the blocks.`, platformGuide)
}

// normalizeSocialPlatforms validates + de-dupes requested platform keys,
// falling back to the default set when none are valid.
func normalizeSocialPlatforms(platforms []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, p := range platforms {
		key := strings.TrimSpace(strings.ToLower(p))
		if _, ok := supportedSocialPlatforms[key]; ok && !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	if len(out) == 0 {
		return append([]string(nil), defaultSocialPlatforms...)
	}
	return out
}

// parseSocialPosts extracts the @@@key@@@ ... @@@end@@@ blocks from the model
// output, in the requested order, sanitizing each.
func parseSocialPosts(raw string, selected []string) []SocialPost {
	byKey := make(map[string]string)
	for key := range supportedSocialPlatforms {
		open := "@@@" + key + "@@@"
		oi := strings.Index(raw, open)
		if oi < 0 {
			continue
		}
		rest := raw[oi+len(open):]
		ci := strings.Index(rest, "@@@end@@@")
		content := rest
		if ci >= 0 {
			content = rest[:ci]
		}
		content = strings.TrimSpace(SanitizeResponse(content))
		if content != "" {
			byKey[key] = content
		}
	}

	var posts []SocialPost
	for _, key := range selected {
		if c, ok := byKey[key]; ok {
			posts = append(posts, SocialPost{
				Platform: key,
				Label:    supportedSocialPlatforms[key].Label,
				Content:  c,
			})
		}
	}
	return posts
}
