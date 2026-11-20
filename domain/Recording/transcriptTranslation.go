package domain

// A line's kept translations: transcript_translations on the line's own node,
// so a purge that drops the line drops them (transcriptPredicates). It holds a
// JSON object by language, each the translated words, the model that made them
// and a hash of the words they were made from, so a line corrected since reads
// as untranslated and is translated again. Only the server writes it, from a
// transcript read (business/TranscriptExport); a read never answers it.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// maxKeptLanguages is the most languages a line keeps; a new one drops the
// one made longest ago.
const maxKeptLanguages = 4

// keptTranslation is one language's translation of a line.
type keptTranslation struct {
	Text  string `json:"t"`
	Model string `json:"m"`
	From  string `json:"h"`
	At    int64  `json:"at"`
}

// KeptTranslation is a line's translations to write: its uid and the whole of
// transcript_translations as it should now read.
type KeptTranslation struct {
	LineUid      string
	Translations string
}

// langKey is lang as a line keeps it.
func langKey(lang string) string { return strings.ToLower(strings.TrimSpace(lang)) }

// keptOf is l's kept translations by language; none when it has none or they
// don't parse.
func keptOf(l *dgraphStruct.DgraphTranscript) map[string]keptTranslation {
	kept := map[string]keptTranslation{}
	if l != nil && l.Translations != "" {
		_ = json.Unmarshal([]byte(l.Translations), &kept)
	}
	return kept
}

// TranslationOf is l's kept translation into lang, and the model that made it,
// when it was made from the words l has now.
func TranslationOf(l *dgraphStruct.DgraphTranscript, lang string) (text, model string, ok bool) {
	k, found := keptOf(l)[langKey(lang)]
	if !found || k.Text == "" || k.From != helpers.SHA256Hex(l.Text) {
		return "", "", false
	}
	return k.Text, k.Model, true
}

// WithTranslation is l's transcript_translations with text, made by model
// from the words l has now, kept for lang.
func WithTranslation(l *dgraphStruct.DgraphTranscript, lang, model, text string) string {
	kept := keptOf(l)
	kept[langKey(lang)] = keptTranslation{Text: text, Model: model, From: helpers.SHA256Hex(l.Text), At: time.Now().UnixNano()}
	for len(kept) > maxKeptLanguages {
		oldest := ""
		for k, v := range kept {
			if oldest == "" || v.At < kept[oldest].At {
				oldest = k
			}
		}
		delete(kept, oldest)
	}
	b, _ := json.Marshal(kept)
	return string(b)
}

// lineUID is a uid as Dgraph writes one.
var lineUID = regexp.MustCompile(`^0x[0-9a-f]+$`)

// keepTranslationsQuery finds the lines of uids that are still transcript
// lines (a purge since the read took them away).
func keepTranslationsQuery(uids []string) string {
	return fmt.Sprintf(`query { l as var(func: uid(%s)) @filter(has(transcript_text)) }`, strings.Join(uids, ", "))
}

// KeepTranslations writes every line's translations in one mutation, and only
// while every line is still there: a line purged since the read is never
// written back as a node holding nothing but its translation.
func KeepTranslations(ctx context.Context, kept []KeptTranslation) error {
	if len(kept) == 0 {
		return nil
	}
	uids := make([]string, 0, len(kept))
	set := make([]map[string]string, 0, len(kept))
	for _, k := range kept {
		if !lineUID.MatchString(k.LineUid) {
			return fmt.Errorf("keep translations: %q is not a line uid", k.LineUid)
		}
		uids = append(uids, k.LineUid)
		set = append(set, map[string]string{"uid": k.LineUid, "transcript_translations": k.Translations})
	}
	_, err := dgexec.Mutate(ctx, keepTranslationsQuery(uids), nil, dgexec.Mutation{Set: set, Cond: fmt.Sprintf("@if(eq(len(l), %d))", len(uids))})
	return err
}
