package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// translation is one ?lang= read's world: the Translator's calls, the texts
// it was sent, and the mutations kept.
type translation struct {
	calls int
	sent  [][]string
	kept  [][]recordingDomain.KeptTranslation
	err   error
}

// withTranslator registers a Translator that upper-cases (or answers tr.err)
// as "qwen3 4B", and counts the keeps, for t.
func withTranslator(t *testing.T, tr *translation) {
	t.Helper()
	wasT, wasK := translator, keepTranslations
	RegisterTranslator(func(_ context.Context, u *userModels.UserInfo, texts []string, lang string) ([]string, string, error) {
		if u == nil || u.UserDgraphInfo.Uuid != "u-1" {
			return nil, "", errors.New("no member")
		}
		tr.calls++
		tr.sent = append(tr.sent, texts)
		if tr.err != nil {
			return nil, "", tr.err
		}
		out := make([]string, len(texts))
		for i, s := range texts {
			out[i] = strings.ToUpper(s) + "@" + lang
		}
		return out, "qwen3 4B", nil
	})
	keepTranslations = func(_ context.Context, k []recordingDomain.KeptTranslation) error {
		tr.kept = append(tr.kept, k)
		return nil
	}
	t.Cleanup(func() { translator, keepTranslations = wasT, wasK })
}

// lines is a recording of n lines "line i", line i's uid 0x(i+1).
func lines(n int) *dgraphStruct.DgraphRecording {
	rec := &dgraphStruct.DgraphRecording{TranscriptCount: uint64(n)}
	for i := 0; i < n; i++ {
		rec.Transcript = append(rec.Transcript, &dgraphStruct.DgraphTranscript{Uid: fmt.Sprintf("0x%x", i+1), Text: fmt.Sprintf("line %d", i)})
	}
	return rec
}

// readAs answers query q for rec as the member u-1: what Answer handed back
// and the response.
func readAs(t *testing.T, q string, rec *dgraphStruct.DgraphRecording) (*dgraphStruct.DgraphRecording, *httptest.ResponseRecorder, int) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/transcript?"+q, nil)
	u := userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uuid: "u-1"}}
	r = r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, u))
	w := httptest.NewRecorder()
	reads := 0
	got := Answer(w, r, helpers.Page{Size: 20}, func(helpers.Page) (*dgraphStruct.DgraphRecording, string) {
		reads++
		return rec, "standup"
	})
	return got, w, reads
}

func TestALangReadOnTheLineWithoutAIAnswers400BeforeAnyRead(t *testing.T) {
	was := translator
	translator = nil
	t.Cleanup(func() { translator = was })
	got, w, reads := readAs(t, "lang=es", lines(2))
	if got != nil || reads != 0 || w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "not in this edition") {
		t.Fatalf("got %v, %d reads, %d %s; want 400 not in this edition and no read", got, reads, w.Code, w.Body.String())
	}
}

func TestALangReadTranslatesItsUntranslatedLinesInOneCallAndKeepsThemInOneMutation(t *testing.T) {
	tr := &translation{}
	withTranslator(t, tr)
	rec := lines(3)
	rec.Transcript[0].Translations = recordingDomain.WithTranslation(rec.Transcript[0], "es", "llama 8B", "KEPT")
	rec.Transcript[1].Translations = recordingDomain.WithTranslation(rec.Transcript[1], "es", "llama 8B", "STALE")
	rec.Transcript[1].Text = "line 1 corrected"
	got, w, reads := readAs(t, "lang=es", rec)
	if got == nil || reads != 1 || w.Code != http.StatusOK {
		t.Fatalf("a ?lang read: got %v after %d reads, %d", got, reads, w.Code)
	}
	if tr.calls != 1 || len(tr.kept) != 1 {
		t.Fatalf("%d calls, %d keeps; want one of each", tr.calls, len(tr.kept))
	}
	if strings.Join(tr.sent[0], "|") != "line 1 corrected|line 2" {
		t.Fatalf("%d calls sent %v; want one call with the corrected line and the untranslated one", tr.calls, tr.sent)
	}
	if len(tr.kept) != 1 || len(tr.kept[0]) != 2 || tr.kept[0][0].LineUid != "0x2" || tr.kept[0][1].LineUid != "0x3" {
		t.Fatalf("kept %v; want one mutation for lines 0x2 and 0x3", tr.kept)
	}
	want := []string{"KEPT", "LINE 1 CORRECTED@es", "LINE 2@es"}
	for i, l := range got.Transcript {
		if l.Translation != want[i] || l.Translations != "" {
			t.Fatalf("line %d answers %q (kept %q); want %q and nothing kept in the answer", i, l.Translation, l.Translations, want[i])
		}
	}
	if got.Translation == nil || *got.Translation != (dgraphStruct.TranscriptTranslation{Language: "es", Model: "qwen3 4B, llama 8B", Pending: 0}) {
		t.Fatalf("the answer's translation is %+v; want es by both models, none pending", got.Translation)
	}
	if text, model, ok := recordingDomain.TranslationOf(&dgraphStruct.DgraphTranscript{Text: "line 1 corrected", Translations: tr.kept[0][0].Translations}, "es"); !ok || text != "LINE 1 CORRECTED@es" || model != "qwen3 4B" {
		t.Fatalf("the kept corrected line reads back %q %q %v", text, model, ok)
	}
}

func TestALangReadTakesAtMostOneBatchAndLeavesTheRestPending(t *testing.T) {
	tr := &translation{}
	withTranslator(t, tr)
	got, _, _ := readAs(t, "lang=es", lines(60))
	if tr.calls != 1 || got == nil || got.Translation == nil {
		t.Fatalf("%d calls, answer %v; want one call", tr.calls, got)
	}
	if len(tr.sent[0]) != MaxTranslatedPerRead || got.Translation.Pending != 60-MaxTranslatedPerRead {
		t.Fatalf("%d calls, %d lines sent, %+v; want one call of %d and the rest pending", tr.calls, len(tr.sent[0]), got.Translation, MaxTranslatedPerRead)
	}
}

func TestALangReadLeavesLinesPendingWhenTheModelMisanswers(t *testing.T) {
	tr := &translation{err: ErrTryAgain}
	withTranslator(t, tr)
	got, w, _ := readAs(t, "lang=es", lines(2))
	if got == nil || w.Code != http.StatusOK || got.Translation == nil || got.Translation.Pending != 2 || len(tr.kept) != 0 {
		t.Fatalf("got %+v, %d, kept %v; want both lines pending and nothing kept", got, w.Code, tr.kept)
	}
	for _, l := range got.Transcript {
		if l.Translation != "" {
			t.Fatalf("a pending line answers %q; want nothing, never a mismatched line", l.Translation)
		}
	}
}

func TestALangReadAnswersTheTranslatorsRefusalAsTheAIOffAnswer(t *testing.T) {
	tr := &translation{err: errors.New("AI is not enabled")}
	withTranslator(t, tr)
	rec := lines(1)
	rec.Transcript[0].Translations = recordingDomain.WithTranslation(rec.Transcript[0], "es", "llama 8B", "KEPT")
	got, w, _ := readAs(t, "lang=es", rec)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if got != nil || w.Code != http.StatusBadRequest || body["msg"] != "AI is not enabled" || strings.Contains(w.Body.String(), "KEPT") {
		t.Fatalf("an AI-off member: %v, %d %s; want today's 400 AI is not enabled and no kept line", got, w.Code, w.Body.String())
	}
}

func TestALangExportNamesTheModelAndWaitsWhileLinesArePending(t *testing.T) {
	tr := &translation{}
	withTranslator(t, tr)
	for _, f := range []string{"txt", "vtt", "srt"} {
		got, w, _ := readAs(t, "format="+f+"&lang=es", lines(2))
		body := w.Body.String()
		if got != nil || w.Code != http.StatusOK || !strings.Contains(body, "Translated by qwen3 4B") || !strings.Contains(body, "LINE 1@es") || strings.Contains(body, "line 1") {
			t.Fatalf("%s export: %d\n%s\nwant the translation with Translated by qwen3 4B in its header", f, w.Code, body)
		}
	}
	tr.err = ErrTryAgain
	_, w, _ := readAs(t, "format=txt&lang=es", lines(3))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusConflict || body["pending"] != float64(3) {
		t.Fatalf("a pending export: %d %s; want 409 with pending 3", w.Code, w.Body.String())
	}
}

func TestAPlainReadNeverAnswersKeptTranslations(t *testing.T) {
	tr := &translation{}
	withTranslator(t, tr)
	rec := lines(1)
	rec.Transcript[0].Translations = `{"es":{"t":"KEPT"}}`
	got, _, _ := readAs(t, "", rec)
	if got == nil || got.Transcript[0].Translations != "" || got.Translation != nil || tr.calls != 0 {
		t.Fatalf("a plain read answered %+v after %d calls; want no kept translations and no call", got.Transcript[0], tr.calls)
	}
}
