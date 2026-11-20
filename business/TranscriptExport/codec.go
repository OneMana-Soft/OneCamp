// Package business writes a call's transcript as a file: WebVTT, SubRip or
// plain text, from the lines one capped read returned (zoom-05). Anyone who
// can read a transcript can export it, on every plan; the route's own read
// check decides who, and this package only formats what that read returned.
// A corrected line exports its words as they read now.
package business

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Format is a file a transcript is written as.
type Format string

const (
	VTT  Format = "vtt"
	SRT  Format = "srt"
	Text Format = "txt"
)

const (
	// MaxLines is the most lines an export carries: the transcript reads' own
	// cap (domain/LiveKit defaultTranscriptLineCap), so one read is the file.
	MaxLines = 5000
	// MaxBytes is the most bytes an export writes; the lines past it are left
	// out and the file says so.
	MaxBytes = 4 << 20
	// tailMs is how long the last line shows, having no next line to end it.
	tailMs = 3000
)

// Line is one line as a file shows it: who said it, when from the call's
// start, and its words.
type Line struct {
	Speaker string
	StartMs int64
	Text    string
}

// ParseFormat is the format a ?format= value names.
func ParseFormat(s string) (Format, bool) {
	switch f := Format(strings.ToLower(s)); f {
	case VTT, SRT, Text:
		return f, true
	}
	return "", false
}

// linesOf are rec's lines as a file shows them, in the read's order: the
// words as they read now (a correction's), the speaker's name, and the start
// from the call's (recordingDomain.LineOffsetMs, as search takes it). A line
// with no words is left out.
func linesOf(rec *dgraphStruct.DgraphRecording) []Line {
	if rec == nil {
		return nil
	}
	var startedMs int64
	if rec.StartedAt != nil {
		startedMs = rec.StartedAt.UnixMilli()
	}
	lines := make([]Line, 0, len(rec.Transcript))
	for _, l := range rec.Transcript {
		if l == nil || strings.TrimSpace(l.Text) == "" {
			continue
		}
		words := l.Text
		if rec.Translation != nil {
			words = l.Translation
		}
		lines = append(lines, Line{Speaker: speakerOf(l.From), StartMs: recordingDomain.LineOffsetMs(l, startedMs), Text: words})
	}
	return lines
}

// speakerOf is a line's speaker as a file names them: their full name, else
// their handle; never an address.
func speakerOf(u *dgraphStruct.DgraphUser) string {
	if u == nil {
		return ""
	}
	if u.UserFullName != "" {
		return u.UserFullName
	}
	return u.UserName
}

// write writes lines as f to w, at most maxBytes of them (the header and a
// closing note aside): truncated when lines were left out to stay under it,
// and the file then says it stops at the size cap. overCap says the read held
// more lines than it took, which the file says at its end.
func write(w io.Writer, f Format, head string, lines []Line, maxBytes int, overCap bool) (bool, error) {
	bw := bufio.NewWriter(w)
	if f == VTT {
		bw.WriteString("WEBVTT\n\n")
	}
	written, lastEnd, n := 0, int64(0), 0
	if head != "" {
		bw.WriteString(note(f, 1, 0, head))
		n = 1
	}
	truncated := false
	for i, l := range lines {
		end := l.StartMs + tailMs
		if i+1 < len(lines) && lines[i+1].StartMs > l.StartMs {
			end = lines[i+1].StartMs
		}
		cue := formatLine(f, n+1, l, end)
		if written+len(cue) > maxBytes {
			truncated = true
			break
		}
		bw.WriteString(cue)
		written += len(cue)
		lastEnd, n = end, n+1
	}
	switch {
	case truncated:
		bw.WriteString(note(f, n+1, lastEnd, "This export stops here, at the file's size cap."))
	case overCap:
		bw.WriteString(note(f, n+1, lastEnd, fmt.Sprintf("This export stops here, at its line cap of %d lines.", MaxLines)))
	}
	return truncated, bw.Flush()
}

// formatLine is line n as f writes it, ending at endMs.
func formatLine(f Format, n int, l Line, endMs int64) string {
	speaker, text := oneLine(l.Speaker), oneLine(l.Text)
	if speaker == "" {
		speaker = "Someone"
	}
	switch f {
	case VTT:
		return fmt.Sprintf("%d\n%s --> %s\n<v %s>%s\n\n", n, clock(l.StartMs, '.'), clock(endMs, '.'), vttEscape(speaker), vttEscape(text))
	case SRT:
		return fmt.Sprintf("%d\n%s --> %s\n%s: %s\n\n", n, clock(l.StartMs, ','), clock(endMs, ','), noArrow(speaker), noArrow(text))
	}
	return fmt.Sprintf("[%s] %s: %s\n", clock(l.StartMs, 0)[:8], speaker, text)
}

// note is a closing line saying the file stops: a NOTE in WebVTT, a last cue
// in SubRip (it has no comments), a last line in text.
func note(f Format, n int, atMs int64, words string) string {
	switch f {
	case VTT:
		return "NOTE " + words + "\n\n"
	case SRT:
		return formatLine(SRT, n, Line{Speaker: "OneCamp", StartMs: atMs, Text: words}, atMs+tailMs)
	}
	return "[" + words + "]\n"
}

// oneLine is s on one line: line breaks and blank lines become one space, so
// no cue ends early.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// vttEscape writes s as WebVTT cue text: &, < and > as entities, so "-->"
// can't end a timing and no tag opens.
func vttEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// noArrow is s with no "-->" in it (SubRip has no escape for it).
func noArrow(s string) string {
	for strings.Contains(s, "-->") {
		s = strings.ReplaceAll(s, "-->", "->")
	}
	return s
}

// clock is ms as hh:mm:ss with sep and milliseconds (sep 0: hh:mm:ss.mmm,
// cut by the caller).
func clock(ms int64, sep byte) string {
	if ms < 0 {
		ms = 0
	}
	if sep == 0 {
		sep = '.'
	}
	return fmt.Sprintf("%02d:%02d:%02d%c%03d", ms/3600000, ms/60000%60, ms/1000%60, sep, ms%1000)
}

// fileName is the file's name: the call's place and date ("launch call
// 2026-11-16.vtt"), in safe characters only, never an address: a place's
// name is cut at an @ and anything outside letters, digits, space, dot, dash
// and underscore becomes a dash.
func fileName(place string, at *time.Time, f Format) string {
	if i := strings.IndexByte(place, '@'); i >= 0 {
		place = place[:i]
	}
	var b strings.Builder
	for _, r := range place {
		switch {
		case r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || r == '.' || r == '_' || r == '-'):
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(strings.Join(strings.Fields(b.String()), " "), " .-")
	if len(name) > 60 {
		name = strings.TrimRight(name[:60], " .-")
	}
	if name != "" {
		name += " "
	}
	name += "call"
	if at != nil && !at.IsZero() {
		name += " " + at.UTC().Format("2006-01-02")
	}
	return name + "." + string(f)
}

// contentTypes are the files' media types.
var contentTypes = map[Format]string{VTT: "text/vtt; charset=utf-8", SRT: "application/x-subrip; charset=utf-8", Text: "text/plain; charset=utf-8"}

// Serve answers the export of rec as f: an attachment named for place and
// the call's date, written as it is formatted from the one read, capped in
// lines (the read's) and in bytes. A transcript longer than the read took
// says so at its end.
func Serve(w http.ResponseWriter, f Format, place string, rec *dgraphStruct.DgraphRecording) {
	var at *time.Time
	if rec != nil {
		at = rec.StartedAt
	}
	w.Header().Set("Content-Type", contentTypes[f])
	w.Header().Set("Content-Disposition", `attachment; filename="`+fileName(place, at, f)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	lines := linesOf(rec)
	overCap := rec != nil && int(rec.TranscriptCount) > len(rec.Transcript)
	head := ""
	if rec != nil && rec.Translation != nil {
		by := ""
		if rec.Translation.Model != "" {
			by = " by " + rec.Translation.Model
		}
		head = "Translated" + by + " into " + rec.Translation.Language + "."
	}
	_, _ = write(w, f, head, lines, MaxBytes, overCap)
}

// PlaceOf is the name an export of rec is filed under: its channel's name,
// else "group" or "direct" for a chat (never a person's name or address),
// else none (an event's call: its route names it for the event).
func PlaceOf(rec *dgraphStruct.DgraphRecording) string {
	switch _, kind := recordingDomain.ConversationOf(rec); kind {
	case "channel":
		if rec.Channel.Name != "" {
			return rec.Channel.Name
		}
		return "channel"
	case "group":
		return "group"
	case "dm":
		return "direct"
	}
	return ""
}

// Answer answers a transcript read and its ?format= export through one read.
// Without ?format= it calls read for the page asked and hands back what read
// gave, for the caller to answer as a page. With ?format= it calls read once
// for the whole transcript (one capped read, never a page loop), answers the
// file and hands back nil. A format it doesn't know is answered 400 before
// any read. read gives the recording and the name of the place it was said
// in, or answers the request itself (a failed read, the missing answer) and
// gives nil; Answer then hands back nil too.
func Answer(w http.ResponseWriter, r *http.Request, page helpers.Page, read func(helpers.Page) (*dgraphStruct.DgraphRecording, string)) *dgraphStruct.DgraphRecording {
	q := r.URL.Query()
	f, refused := fromQuery(w, q)
	if refused {
		return nil
	}
	lang, refused := langOf(w, q)
	if refused {
		return nil
	}
	if f != "" {
		page = helpers.Page{Size: MaxLines}
	}
	rec, place := read(page)
	if rec == nil {
		return nil
	}
	if lang != "" && !translate(w, r, rec, lang) {
		return nil
	}
	unkept(rec)
	if f == "" {
		return rec
	}
	if rec.Translation != nil && rec.Translation.Pending > 0 {
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "Some lines are still being translated. Try again in a moment.", "pending": rec.Translation.Pending})
		return nil
	}
	Serve(w, f, place, rec)
	return nil
}

// fromQuery is the export a transcript read's ?format= asks for: "" when it
// asks none (the read answers a page, as before). A format it doesn't know
// is answered 400 here, and refused is true.
func fromQuery(w http.ResponseWriter, q url.Values) (f Format, refused bool) {
	if !q.Has("format") {
		return "", false
	}
	f, ok := ParseFormat(q.Get("format"))
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Ask for format=vtt, srt or txt."})
		return "", true
	}
	return f, false
}
