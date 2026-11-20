package business

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// goldenLines cover the codec's cases: escaping (&, <, >, "-->"), blank lines
// inside a line's words, two lines starting together, an hour-long offset,
// and a speaker with no name.
var goldenLines = []Line{
	{Speaker: "Sam Rivera", StartMs: 1500, Text: "The budget is ninety thousand"},
	{Speaker: "Maya & <Co>", StartMs: 4200, Text: "a --> b <tag>\n\nand more"},
	{Speaker: "Jo", StartMs: 4200, Text: "same start"},
	{Speaker: "", StartMs: 3723004, Text: "an hour in"},
}

var (
	vttTiming = regexp.MustCompile(`^(\d{2}):(\d{2}):(\d{2})\.(\d{3}) --> (\d{2}):(\d{2}):(\d{2})\.(\d{3})$`)
	srtTiming = regexp.MustCompile(`^(\d{2}):(\d{2}):(\d{2}),(\d{3}) --> (\d{2}):(\d{2}):(\d{2}),(\d{3})$`)
)

func msOf(m []string, from int) int64 {
	h, _ := strconv.Atoi(m[from])
	mi, _ := strconv.Atoi(m[from+1])
	s, _ := strconv.Atoi(m[from+2])
	ms, _ := strconv.Atoi(m[from+3])
	return int64(((h*60+mi)*60+s)*1000 + ms)
}

// validCues checks a VTT or SRT body: cues apart by one blank line, each a
// number counting from 1, a timing line whose end is after its start, and
// one non-empty text line holding no "-->". VTT starts with its header, and
// may carry NOTE blocks.
func validCues(t *testing.T, body string, f Format) int {
	t.Helper()
	timing := srtTiming
	if f == VTT {
		timing = vttTiming
		if !strings.HasPrefix(body, "WEBVTT\n\n") {
			t.Fatalf("a WebVTT file starts with its header: %q", body[:min(len(body), 20)])
		}
		body = strings.TrimPrefix(body, "WEBVTT\n\n")
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("every cue ends with a blank line")
	}
	n := 0
	for _, block := range strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n") {
		if f == VTT && strings.HasPrefix(block, "NOTE ") {
			if strings.Contains(block, "-->") {
				t.Fatalf("a NOTE holds no -->: %q", block)
			}
			continue
		}
		rows := strings.Split(block, "\n")
		if len(rows) != 3 {
			t.Fatalf("a cue is a number, a timing and one text line: %q", block)
		}
		n++
		if rows[0] != strconv.Itoa(n) {
			t.Fatalf("cue %d is numbered %q", n, rows[0])
		}
		m := timing.FindStringSubmatch(rows[1])
		if m == nil || msOf(m, 5) <= msOf(m, 1) {
			t.Fatalf("cue %d's timing %q", n, rows[1])
		}
		if strings.TrimSpace(rows[2]) == "" || strings.Contains(rows[2], "-->") {
			t.Fatalf("cue %d's text %q", n, rows[2])
		}
	}
	return n
}

// M7: VTT, SRT and text match their golden files and pass the validator.
func TestATranscriptExportsAsWebVTTSubRipAndTextByItsGoldenFiles(t *testing.T) {
	for _, f := range []Format{VTT, SRT, Text} {
		var out bytes.Buffer
		truncated, err := write(&out, f, "", goldenLines, MaxBytes, false)
		if err != nil || truncated {
			t.Fatalf("%s: truncated %v, %v", f, truncated, err)
		}
		want, err := os.ReadFile("testdata/lines." + string(f) + ".golden")
		if err != nil {
			t.Fatal(err)
		}
		if out.String() != string(want) {
			t.Fatalf("%s differs from its golden file:\n%s\n-- want --\n%s", f, out.String(), want)
		}
		if f != Text {
			if n := validCues(t, out.String(), f); n != len(goldenLines) {
				t.Fatalf("%s carries %d cues of %d lines", f, n, len(goldenLines))
			}
		}
	}
}

// M7: every line up to the cap, in order, each its own cue.
func TestAnExportCarriesEveryLineInOrderUpToTheCap(t *testing.T) {
	lines := make([]Line, MaxLines)
	for i := range lines {
		lines[i] = Line{Speaker: "Sam", StartMs: int64(i) * 1000, Text: fmt.Sprintf("line %d", i)}
	}
	var out bytes.Buffer
	if truncated, err := write(&out, SRT, "", lines, MaxBytes, false); err != nil || truncated {
		t.Fatalf("truncated %v, %v", truncated, err)
	}
	if n := validCues(t, out.String(), SRT); n != MaxLines {
		t.Fatalf("%d cues of %d lines", n, MaxLines)
	}
	if strings.Index(out.String(), "Sam: line 4998\n") > strings.Index(out.String(), "Sam: line 4999\n") {
		t.Fatal("the lines are out of order")
	}
}

// M13: the bytes are capped too; the file stays valid and says it stops.
func TestAnExportStopsAtItsByteCapAndSaysSo(t *testing.T) {
	lines := make([]Line, 200)
	for i := range lines {
		lines[i] = Line{Speaker: "Sam", StartMs: int64(i) * 1000, Text: strings.Repeat("word ", 20)}
	}
	for _, f := range []Format{VTT, SRT, Text} {
		var out bytes.Buffer
		truncated, err := write(&out, f, "", lines, 2000, false)
		if err != nil || !truncated {
			t.Fatalf("%s: truncated %v, %v", f, truncated, err)
		}
		if out.Len() > 2000+300 {
			t.Fatalf("%s wrote %d bytes past a 2000-byte cap", f, out.Len())
		}
		if !strings.Contains(out.String(), "size cap") {
			t.Fatalf("%s doesn't say it stops at the size cap", f)
		}
		if f != Text {
			validCues(t, out.String(), f)
		}
	}
}

// M7: the export takes each line's words as they read now (a correction is
// the line's text), its speaker's name, and its offset from the call start.
func TestLinesOfTakesTheCorrectedWordsTheSpeakerAndTheOffset(t *testing.T) {
	started := time.UnixMilli(1_000_000)
	ts, off := int64(1_004_000), int64(2500)
	at := time.Now()
	rec := &dgraphStruct.DgraphRecording{StartedAt: &started, Transcript: []*dgraphStruct.DgraphTranscript{
		{Uid: "0x1", Text: "the corrected words", OriginalText: "the filed words", CorrectedAt: &at, OffsetMs: &off,
			From: &dgraphStruct.DgraphUser{UserName: "sam", UserFullName: "Sam Rivera"}},
		{Uid: "0x2", Text: "legacy line", TimeStamp: &ts, From: &dgraphStruct.DgraphUser{UserName: "maya"}},
		{Uid: "0x3", Text: "   "},
	}}
	got := linesOf(rec)
	want := []Line{{Speaker: "Sam Rivera", StartMs: 2500, Text: "the corrected words"}, {Speaker: "maya", StartMs: 4000, Text: "legacy line"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// M13: the file is named for the call's place and date, safely, and never
// carries an address.
func TestAnExportsFileNameNamesTheCallAndItsDateSafely(t *testing.T) {
	at := time.Date(2026, 11, 16, 14, 5, 0, 0, time.UTC)
	if got := fileName("launch", &at, VTT); got != "launch call 2026-11-16.vtt" {
		t.Fatalf("got %q", got)
	}
	got := fileName(`sam@example.com "/../x";`, &at, SRT)
	if strings.ContainsAny(got, `@/\";`) || strings.Contains(got, "example") || !strings.HasSuffix(got, " call 2026-11-16.srt") {
		t.Fatalf("unsafe name %q", got)
	}
	if got := fileName("", nil, Text); got != "call.txt" {
		t.Fatalf("no place, no date: %q", got)
	}
}

// Serve answers an attachment of the right type, and the words corrected.
func TestServeAnswersAnAttachmentOfItsFormat(t *testing.T) {
	at := time.Date(2026, 11, 16, 14, 5, 0, 0, time.UTC)
	off := int64(0)
	rec := &dgraphStruct.DgraphRecording{StartedAt: &at, TranscriptCount: 1, Transcript: []*dgraphStruct.DgraphTranscript{
		{Uid: "0x1", Text: "fixed", OriginalText: "fxed", OffsetMs: &off, From: &dgraphStruct.DgraphUser{UserName: "sam"}}}}
	w := httptest.NewRecorder()
	Serve(w, VTT, "launch", rec)
	if ct := w.Header().Get("Content-Type"); ct != "text/vtt; charset=utf-8" {
		t.Fatalf("content type %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename="launch call 2026-11-16.vtt"` {
		t.Fatalf("disposition %q", cd)
	}
	if !strings.Contains(w.Body.String(), "<v sam>fixed\n") || strings.Contains(w.Body.String(), "fxed") {
		t.Fatalf("body %q", w.Body.String())
	}
}

// M13: a transcript longer than the read's cap says the export stops there.
func TestServeSaysWhenTheTranscriptIsLongerThanTheCap(t *testing.T) {
	off := int64(0)
	rec := &dgraphStruct.DgraphRecording{TranscriptCount: MaxLines + 7, Transcript: []*dgraphStruct.DgraphTranscript{
		{Uid: "0x1", Text: "first", OffsetMs: &off}}}
	w := httptest.NewRecorder()
	Serve(w, Text, "launch", rec)
	if !strings.Contains(w.Body.String(), "line cap") {
		t.Fatalf("body %q", w.Body.String())
	}
}

func TestParseFormatTakesTheThreeFormats(t *testing.T) {
	for s, want := range map[string]Format{"vtt": VTT, "srt": SRT, "txt": Text, "VTT": VTT} {
		if got, ok := ParseFormat(s); !ok || got != want {
			t.Fatalf("%q: %q %v", s, got, ok)
		}
	}
	for _, s := range []string{"", "json", "pdf"} {
		if _, ok := ParseFormat(s); ok {
			t.Fatalf("%q is no format", s)
		}
	}
}
