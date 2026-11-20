package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	business "github.com/akashc777/OneCamp/business/AI"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// POST /ai/translate (zoom-05 C1): texts[] is translated as one batch, the
// translations answered in the order sent beside the model's name; text is
// the one-text form. A model that answers the wrong number of lines twice
// (business/AI ErrTranslateTryAgain) is 503 "try again", never a 400 that
// reads as the asker's mistake. A body over the cap is 400 before any call.

type translateCall struct {
	texts []string
	lang  string
}

func stubTranslate(t *testing.T, err error) *[]translateCall {
	t.Helper()
	savedLines, savedText := translateLines, translateText
	t.Cleanup(func() { translateLines, translateText = savedLines, savedText })
	var calls []translateCall
	translateLines = func(_ context.Context, _ *userModels.UserInfo, texts []string, lang string) ([]string, string, error) {
		calls = append(calls, translateCall{texts, lang})
		if err != nil {
			return nil, "", err
		}
		out := make([]string, len(texts))
		for i, s := range texts {
			out[i] = "fr:" + s
		}
		return out, "qwen3 4B", nil
	}
	translateText = func(_ context.Context, _ *userModels.UserInfo, text, lang string) (string, string, error) {
		calls = append(calls, translateCall{[]string{text}, "text:" + lang})
		if err != nil {
			return "", "", err
		}
		return "fr:" + text, "qwen3 4B", nil
	}
	return &calls
}

type translateAnswer struct {
	Msg  string `json:"msg"`
	Data struct {
		Translations []string `json:"translations"`
		Translation  *string  `json:"translation"`
		Model        string   `json:"model"`
	} `json:"data"`
}

func postTranslate(t *testing.T, body any) (int, translateAnswer) {
	t.Helper()
	w := httptest.NewRecorder()
	Translate(w, docAIRequest(t, "/ai/translate", body))
	var got translateAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("answer is not JSON (%d): %v", w.Code, err)
	}
	return w.Code, got
}

func TestTranslateTextsAnswersTheTranslationsInTheOrderSentAndTheModel(t *testing.T) {
	calls := stubTranslate(t, nil)
	code, got := postTranslate(t, map[string]any{"texts": []string{"one", "two", "three"}, "target_language": " French "})
	if code != http.StatusOK {
		t.Fatalf("status %d (%s), want 200", code, got.Msg)
	}
	if want := []string{"fr:one", "fr:two", "fr:three"}; !reflect.DeepEqual(got.Data.Translations, want) {
		t.Errorf("translations %q, want %q in the order sent", got.Data.Translations, want)
	}
	if got.Data.Model != "qwen3 4B" || got.Data.Translation != nil {
		t.Errorf("model %q, translation %v; want the model's name and no one-text field", got.Data.Model, got.Data.Translation)
	}
	if want := []translateCall{{[]string{"one", "two", "three"}, "French"}}; !reflect.DeepEqual(*calls, want) {
		t.Errorf("translator asked %+v, want one batch in order %+v", *calls, want)
	}
}

func TestTranslateTextIsTheOneTextForm(t *testing.T) {
	calls := stubTranslate(t, nil)
	code, got := postTranslate(t, map[string]any{"text": "one", "target_language": "French"})
	if code != http.StatusOK || got.Data.Translation == nil || *got.Data.Translation != "fr:one" ||
		got.Data.Model != "qwen3 4B" || got.Data.Translations != nil {
		t.Fatalf("status %d, answer %+v; want 200 with translation fr:one and the model", code, got.Data)
	}
	if len(*calls) != 1 || (*calls)[0].lang != "text:French" {
		t.Errorf("translator asked %+v, want the one-text form once", *calls)
	}
}

func TestTranslateAModelThatMiscountsTwiceIs503TryAgain(t *testing.T) {
	stubTranslate(t, business.ErrTranslateTryAgain)
	for _, body := range []map[string]any{
		{"texts": []string{"one", "two"}, "target_language": "French"},
		{"text": "one", "target_language": "French"},
	} {
		code, got := postTranslate(t, body)
		if code != http.StatusServiceUnavailable || !strings.Contains(got.Msg, "try again") {
			t.Errorf("%v: status %d %q, want 503 that says try again", body, code, got.Msg)
		}
	}
}

func TestTranslateRefusesAnOversizedBodyOrTooManyTextsAs400(t *testing.T) {
	calls := stubTranslate(t, business.ErrTranslateBounds)
	code, got := postTranslate(t, map[string]any{"texts": []string{strings.Repeat("a", maxTranslateBody)}})
	if code != http.StatusBadRequest || len(*calls) != 0 {
		t.Errorf("a body over %d bytes: status %d %q after %d calls; want 400 before any call", maxTranslateBody, code, got.Msg, len(*calls))
	}
	code, got = postTranslate(t, map[string]any{"texts": make([]string, business.MaxTranslateTexts+1)})
	if code != http.StatusBadRequest || got.Msg != business.ErrTranslateBounds.Error() {
		t.Errorf("too many texts: status %d %q; want 400 with the bounds", code, got.Msg)
	}
}
