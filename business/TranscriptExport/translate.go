package business

// A transcript read in another language (?lang=): the caller's read, as it
// is, with its lines in that language by the member's own model. A line keeps
// its translation (domain/Recording KeepTranslations), so the next reader in
// that language is answered from the read alone; a line nobody translated
// yet, or corrected since, goes to the model, at most one bounded batch a
// request, and the rest wait (pending) for the next read. This package holds
// no AI: the edition with AI registers its Translator; without one a ?lang=
// read is answered 400.
//
// Who may: whoever the caller's read lets read the transcript, AND whom the
// Translator lets use AI (it answers the member's AI-off words otherwise, even
// when every line is kept). Store calls: the read, and at most one Dgraph
// mutation (the new translations) a request.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

const (
	// MaxTranslatedPerRead is the most lines one request sends to be
	// translated, and the most texts POST /ai/translate takes.
	MaxTranslatedPerRead = 50
	// MaxTranslatedBytesPerRead is the most bytes of words one request sends
	// to be translated: a local 4B model's comfortable prompt.
	MaxTranslatedBytesPerRead = 8000
	// maxLangLen is the longest ?lang= taken: a name ("Brazilian Portuguese")
	// or a BCP-47 tag.
	maxLangLen = 35
)

// Translator translates texts into lang as user: the translations in order
// and the model's name. With no texts it answers whether user may use AI and
// their model. Its error is in words a client is shown, or ErrTryAgain.
type Translator func(ctx context.Context, user *userModels.UserInfo, texts []string, lang string) ([]string, string, error)

// ErrTryAgain is a Translator's model answering lines that don't match the
// ones sent: they wait for the next read.
var ErrTryAgain = errors.New("the lines wait for the next read")

// translator is the edition's Translator; nil on the line without AI.
var translator Translator

// RegisterTranslator makes t the Translator ?lang= reads use, and gives back
// the one it replaces.
func RegisterTranslator(t Translator) (was Translator) {
	was, translator = translator, t
	return was
}

// keepTranslations writes a read's new translations (a seam for tests).
var keepTranslations = recordingDomain.KeepTranslations

// langOf is the language a read's ?lang= asks for: "" when it asks none. One
// this edition can't translate into, or a malformed one, is answered 400 here
// before any read, and refused is true.
func langOf(w http.ResponseWriter, q url.Values) (lang string, refused bool) {
	if !q.Has("lang") {
		return "", false
	}
	if translator == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Translation is not in this edition."})
		return "", true
	}
	lang = strings.TrimSpace(q.Get("lang"))
	ok := lang != "" && len(lang) <= maxLangLen
	for _, c := range lang {
		ok = ok && (unicode.IsLetter(c) || unicode.IsDigit(c) || c == '-' || c == '_' || c == ' ')
	}
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Ask for lang= a language's name or code."})
		return "", true
	}
	return lang, false
}

// translate puts rec's lines into lang for the member on r: kept translations
// of the words a line has now, then at most one batch of the rest by the
// Translator, kept in one mutation; the lines left wait. rec says the language,
// the models and how many wait. A Translator's refusal (AI off, its limits)
// is answered 400 here in its words, and ok is false.
func translate(w http.ResponseWriter, r *http.Request, rec *dgraphStruct.DgraphRecording, lang string) (ok bool) {
	ctx := r.Context()
	user, _ := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var send []string
	var sent []*dgraphStruct.DgraphTranscript
	var models []string
	pending, size := 0, 0
	for _, l := range rec.Transcript {
		if l == nil || strings.TrimSpace(l.Text) == "" {
			continue
		}
		if text, model, kept := recordingDomain.TranslationOf(l, lang); kept {
			l.Translation, models = text, withModel(models, model)
			continue
		}
		if len(send) < MaxTranslatedPerRead && size+len(l.Text) <= MaxTranslatedBytesPerRead {
			send, sent, size = append(send, l.Text), append(sent, l), size+len(l.Text)
			continue
		}
		pending++
	}
	out, model, err := translator(ctx, &user, send, lang)
	switch {
	case errors.Is(err, ErrTryAgain):
		pending += len(send)
	case err != nil:
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return false
	default:
		kept := make([]recordingDomain.KeptTranslation, 0, len(sent))
		for i, l := range sent {
			kept = append(kept, recordingDomain.KeptTranslation{LineUid: l.Uid, Translations: recordingDomain.WithTranslation(l, lang, model, out[i])})
			l.Translation = out[i]
		}
		if len(kept) > 0 {
			if err := keepTranslations(ctx, kept); err != nil {
				// The answer still carries them; the next read translates them again.
				helpers.LogErrorWithContext(ctx, "transcriptExport: keeping %d translations failed: %v", len(kept), err)
			}
		}
		// A call with no lines names no model (it reads no store): an answer
		// with nothing kept or sent names none.
		if model != "" && (len(sent) > 0 || len(models) == 0) {
			models = append([]string{model}, without(models, model)...)
		}
	}
	rec.Translation = &dgraphStruct.TranscriptTranslation{Language: lang, Model: strings.Join(models, ", "), Pending: pending}
	return true
}

// withModel is models with m once, in the order first seen.
func withModel(models []string, m string) []string {
	if m == "" {
		return models
	}
	for _, x := range models {
		if x == m {
			return models
		}
	}
	return append(models, m)
}

// without is models without m.
func without(models []string, m string) []string {
	out := models[:0:0]
	for _, x := range models {
		if x != m {
			out = append(out, x)
		}
	}
	return out
}

// unkept clears every line's kept translations: a read never answers them.
func unkept(rec *dgraphStruct.DgraphRecording) {
	for _, l := range rec.Transcript {
		if l != nil {
			l.Translations = ""
		}
	}
}
