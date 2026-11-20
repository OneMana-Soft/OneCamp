package business

// AI translation — translate any text (a chat/channel message, a snippet) into
// a target language, through the shared AI chokepoint. Generic and stateless:
// any caller passes text + a target language (a name like "Spanish" or a BCP-47
// code like "es"/"en-US") and gets the translation back. Runs AS the requesting
// member (their admin-authorized model pick, residency, breaker, rate limit,
// daily token caps) exactly like AskAI, since it's an interactive user action.
// The input text is treated as data, never instructions (injection-hardened).

import (
	"context"
	"fmt"
	"strings"

	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// TranslateText translates text into targetLanguage: TranslateLines of one
// text. targetLanguage may be a language name ("Spanish") or a BCP-47 tag
// ("es", "en-US"); blank is English. It answers the translation and the
// model's name, or a user-safe error when AI is off, the input is empty or
// over the bounds, or the model/limits reject the call.
func TranslateText(ctx context.Context, userInfo *userModels.UserInfo, text, targetLanguage string) (string, string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", "", fmt.Errorf("there's no text to translate")
	}
	out, model, err := TranslateLines(ctx, userInfo, []string{text}, strings.TrimSpace(targetLanguage))
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(out[0]) == "" {
		return "", "", fmt.Errorf("the model returned an empty translation")
	}
	return out[0], model, nil
}
