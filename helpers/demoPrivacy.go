package helpers

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/felixge/httpsnoop"
)

// Everyone who opens the public demo signs in as one shared visitor, so
// anything the visitor can read, any anonymous stranger can read. The demo's
// real members (the people who run it) signed up with their own addresses, and
// every list of people the app serves carries user_email_id, so the @mention
// popup alone used to show those addresses to whoever opened the demo.
//
// Rather than teach each of the hundreds of handlers and Dgraph queries that
// return a person to leave the address out, the auth middleware serves the
// visitor through ServeHidingEmails. In every JSON answer and every stream of
// server-sent events, a string that is exactly an email address (a search's
// highlight of one included) is sent as "", and so is an address in quotes
// inside a string (JSON kept as text), unless it is the visitor's own. Object
// keys stay as they are, and so does an address
// written in running text: a message that mentions one is content like any
// other. Every other body (a file, an image, a CSV export, a page) passes
// through byte for byte, and so does a compressed one.
//
// It is keyed on the visitor alone. The demo's own members, signed in as
// themselves, and every member of every other server see addresses as they
// always have: inside a real workspace people can see each other's email.

// BlankEmails returns body, JSON, with every address but keep blanked as
// ServeHidingEmails blanks them. When nothing needs blanking the same slice
// comes back.
func BlankEmails(body []byte, keep string) []byte {
	if bytes.IndexByte(body, '@') < 0 {
		return body
	}
	b := emailBlanker{keep: []byte(strings.TrimSpace(keep))}
	return b.scan(body, true)
}

// DemoMqttPayload is what a DEMO_MODE server publishes in place of payload to
// a topic the shared visitor may be subscribed to: every address but the
// visitor's own blanked (BlankEmails). On any other server the payload goes
// out as it is.
func DemoMqttPayload(payload interface{}) interface{} {
	if !DemoMode() {
		return payload
	}
	keep := os.Getenv("DEMO_USER_EMAIL")
	switch p := payload.(type) {
	case []byte:
		return BlankEmails(p, keep)
	case string:
		return string(BlankEmails([]byte(p), keep))
	}
	return payload
}

// blankString returns what the raw content of a JSON string becomes, and
// false when it stays as it is: "" for an address, highlighted or not, and an
// address in quotes inside it (\"someone@example.com\") taken out of its
// quotes.
func blankString(val, keep []byte) ([]byte, bool) {
	if bytes.IndexByte(val, '@') < 0 {
		return nil, false
	}
	esc := bytes.IndexByte(val, '\\')
	if esc < 0 && isEmail(val) {
		return nil, !bytes.EqualFold(val, keep)
	}
	if addr, ok := unmarked(val, esc); ok && isEmail(addr) {
		return nil, !bytes.EqualFold(addr, keep)
	}
	if esc < 0 {
		return nil, false
	}
	var out []byte
	last, changed := 0, false
	for i := 0; i < len(val); {
		q := bytes.IndexByte(val[i:], '"')
		if q < 0 {
			break
		}
		// Inside a string every quote is escaped: one that opens a quoted
		// address is followed by it and then a backslash or more and a quote.
		start := i + q + 1
		end := start
		for end < len(val) && val[end] != '\\' && val[end] != '"' {
			end++
		}
		i = end
		k := end
		for k < len(val) && val[k] == '\\' {
			k++
		}
		if k == end || k == len(val) || val[k] != '"' || !isEmail(val[start:end]) || bytes.EqualFold(val[start:end], keep) {
			continue
		}
		i = k + 1
		n := i
		for n < len(val) && val[n] == ' ' {
			n++
		}
		if n < len(val) && val[n] == ':' {
			continue // a key
		}
		out = append(out, val[last:start]...)
		last, changed = end, true
	}
	if !changed {
		return nil, false
	}
	return append(out, val[last:]...), true
}

// The tags a search's highlight puts around what it matched
// (<mark>someone@example.com</mark>), as they are and as encoding/json
// escapes them.
const (
	markOpen         = "<mark>"
	markClose        = "</mark>"
	markOpenEscaped  = `\u003cmark\u003e`
	markCloseEscaped = `\u003c/mark\u003e`
)

// unmarked returns val without the tags of a search's highlight, and false
// when it has none, or has a < or an escape that is not a tag's: an address
// has neither, so val is then no address, highlighted or not. esc is where
// val's first escape is, or -1.
func unmarked(val []byte, esc int) ([]byte, bool) {
	i := esc
	if i < 0 {
		i = bytes.IndexByte(val, '<')
	}
	if i < 0 || markTagLen(val[i:]) == 0 {
		return nil, false
	}
	addr := make([]byte, 0, len(val))
	last := 0
	for i = 0; i < len(val); i++ {
		if val[i] != '<' && val[i] != '\\' {
			continue
		}
		n := markTagLen(val[i:])
		if n == 0 {
			return nil, false
		}
		addr = append(addr, val[last:i]...)
		last = i + n
		i = last - 1
	}
	return append(addr, val[last:]...), true
}

// markTagLen is the length of the highlight tag b starts with, or 0. Compared
// as constants, so a string that starts none costs next to nothing.
func markTagLen(b []byte) int {
	switch {
	case len(b) >= len(markOpen) && string(b[:len(markOpen)]) == markOpen:
		return len(markOpen)
	case len(b) >= len(markClose) && string(b[:len(markClose)]) == markClose:
		return len(markClose)
	case len(b) >= len(markOpenEscaped) && string(b[:len(markOpenEscaped)]) == markOpenEscaped:
		return len(markOpenEscaped)
	case len(b) >= len(markCloseEscaped) && string(b[:len(markCloseEscaped)]) == markCloseEscaped:
		return len(markCloseEscaped)
	}
	return 0
}

// isEmail reports whether b is an email address: a local part of letters,
// digits and ._%+'- , an @, and a domain of two or more labels whose last is
// two letters or more.
func isEmail(b []byte) bool {
	at := bytes.IndexByte(b, '@')
	if at <= 0 {
		return false
	}
	for _, c := range b[:at] {
		if !isAlnum(c) && strings.IndexByte("._%+'-", c) < 0 {
			return false
		}
	}
	domain := b[at+1:]
	dot := bytes.LastIndexByte(domain, '.')
	if dot <= 0 || len(domain)-dot-1 < 2 {
		return false
	}
	for _, c := range domain[dot+1:] {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z') {
			return false
		}
	}
	prev := byte('.')
	for _, c := range domain[:dot] {
		if c == '.' {
			if prev == '.' {
				return false // an empty label
			}
		} else if !isAlnum(c) && c != '-' {
			return false
		}
		prev = c
	}
	return prev != '.'
}

func isAlnum(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

// maxHeld is the longest string held back while it arrives in pieces. One
// longer is no address; it goes out as it comes.
const maxHeld = 1 << 20

// emailBlanker blanks addresses (blankString) in JSON that may arrive in
// pieces, as a stream or a copied file does. A string cut by the end of one
// piece is held back and looked at whole with the next, so where a body is
// cut never decides what it shows.
type emailBlanker struct {
	keep []byte
	held []byte // from the opening quote of a string not yet seen whole
	// What has gone out ends inside a string, because a flush could not wait
	// for its end; esc: right after a backslash in it.
	inStr, esc bool
}

// feed returns what of p can go out now.
func (b *emailBlanker) feed(p []byte) []byte { return b.scan(p, false) }

// drain returns everything held back, for a flush or the end of the answer.
func (b *emailBlanker) drain() []byte { return b.scan(nil, true) }

func (b *emailBlanker) scan(p []byte, drain bool) []byte {
	data := p
	if len(b.held) > 0 {
		data = append(b.held, p...)
		b.held = nil
	}
	i := 0
	// The rest of a string whose start has gone out passes on unseen.
	for b.inStr && i < len(data) {
		c := data[i]
		i++
		switch {
		case b.esc:
			b.esc = false
		case c == '\\':
			b.esc = true
		case c == '"':
			b.inStr = false
		}
	}
	if b.inStr {
		return data
	}

	var out []byte
	last, end, changed := 0, len(data), false
	for i < len(data) {
		q := bytes.IndexByte(data[i:], '"')
		if q < 0 {
			break
		}
		start := i + q
		closing := closingQuote(data, start)
		if closing < 0 {
			// The string runs on past this piece.
			if drain || len(data)-start > maxHeld {
				b.inStr, b.esc = true, endsEscaped(data, start)
			} else {
				end = start
			}
			break
		}
		k := closing + 1
		for k < len(data) && (data[k] == ' ' || data[k] == '\t' || data[k] == '\n' || data[k] == '\r') {
			k++
		}
		if k == len(data) && !drain {
			// Whether it is a key or a value comes with the next piece.
			end = start
			break
		}
		i = closing + 1
		if k < len(data) && data[k] == ':' {
			continue
		}
		if repl, ok := blankString(data[start+1:closing], b.keep); ok {
			if out == nil {
				out = make([]byte, 0, len(data))
			}
			out = append(out, data[last:start+1]...)
			out = append(out, repl...)
			last, changed = closing, true
		}
	}
	if end < len(data) {
		b.held = append([]byte(nil), data[end:]...)
	}
	if !changed {
		return data[:end]
	}
	return append(out, data[last:end]...)
}

// closingQuote is where the string opened at start ends, or -1 when it runs
// past the end of data.
func closingQuote(data []byte, start int) int {
	j := start + 1
	for {
		r := bytes.IndexByte(data[j:], '"')
		if r < 0 {
			return -1
		}
		j += r
		n := 0
		for k := j - 1; k > start && data[k] == '\\'; k-- {
			n++
		}
		if n%2 == 0 {
			return j
		}
		j++
	}
}

// endsEscaped reports whether data ends inside an escape of the string opened
// at start.
func endsEscaped(data []byte, start int) bool {
	n := 0
	for k := len(data) - 1; k > start && data[k] == '\\'; k-- {
		n++
	}
	return n%2 == 1
}

const (
	bodyUndecided = iota
	bodyScanned   // JSON or server-sent events: addresses are blanked
	bodyRaw       // anything else: passed through as it is
)

// emailHider is the visitor's side of ServeHidingEmails.
type emailHider struct {
	w          http.ResponseWriter
	b          emailBlanker
	body       int
	headerSent bool
}

type hidingEmails struct{}

// ServeHidingEmails serves r with next. When the person signed in is the
// demo's shared visitor (IsDemoVisitor), everyone else's address is blanked
// from the answer on its way out, as the top of this file says; for anyone
// else, on any server, next serves w itself and nothing changes.
//
// The writer next gets offers exactly what w offers (http.Flusher,
// http.Hijacker, io.ReaderFrom), so streams still stream, a hijacked
// connection is the real one, and a file served from disk still goes by
// sendfile.
func ServeHidingEmails(next http.Handler, w http.ResponseWriter, r *http.Request, viewerEmail string) {
	if !IsDemoVisitor(viewerEmail) || r.Context().Value(hidingEmails{}) != nil {
		next.ServeHTTP(w, r)
		return
	}
	h := &emailHider{w: w, b: emailBlanker{keep: []byte(strings.TrimSpace(viewerEmail))}}
	defer h.flushHeld()
	wrapped := httpsnoop.Wrap(w, httpsnoop.Hooks{
		WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(code int) {
				h.beforeHeader(code)
				next(code)
			}
		},
		Write: func(httpsnoop.WriteFunc) httpsnoop.WriteFunc { return h.write },
		Flush: func(next httpsnoop.FlushFunc) httpsnoop.FlushFunc {
			return func() {
				h.flushHeld()
				next()
			}
		},
		ReadFrom: func(next httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
			return func(src io.Reader) (int64, error) {
				h.decide(nil)
				if h.body == bodyRaw {
					h.headerSent = true
					return next(src)
				}
				return io.Copy(writerFunc(h.write), src)
			}
		},
	})
	next.ServeHTTP(wrapped, r.WithContext(context.WithValue(r.Context(), hidingEmails{}, true)))
}

// decide settles, once, whether the body is read: by its type, or when it
// has none, by whether it opens as JSON.
func (h *emailHider) decide(p []byte) {
	if h.body != bodyUndecided {
		return
	}
	header := h.w.Header()
	if ce := header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		h.body = bodyRaw // compressed: nothing here can read it
		return
	}
	ct := strings.ToLower(header.Get("Content-Type"))
	switch {
	case strings.Contains(ct, "json"), strings.HasPrefix(ct, "text/event-stream"):
		h.body = bodyScanned
	case ct != "":
		h.body = bodyRaw
	default:
		if t := bytes.TrimLeft(p, " \t\r\n"); len(t) > 0 {
			if t[0] == '{' || t[0] == '[' {
				h.body = bodyScanned
			} else {
				h.body = bodyRaw
			}
		}
	}
}

func (h *emailHider) beforeHeader(code int) {
	if h.headerSent || (code >= 100 && code < 200 && code != http.StatusSwitchingProtocols) {
		return
	}
	h.headerSent = true
	h.decide(nil)
	if h.body != bodyRaw {
		h.w.Header().Del("Content-Length") // blanking shortens the body
	}
}

func (h *emailHider) write(p []byte) (int, error) {
	if !h.headerSent {
		h.headerSent = true
		h.decide(p)
		if h.body != bodyRaw {
			h.w.Header().Del("Content-Length")
		}
	} else {
		h.decide(p)
	}
	if h.body != bodyScanned {
		return h.w.Write(p)
	}
	if out := h.b.feed(p); len(out) > 0 {
		if _, err := h.w.Write(out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// flushHeld sends what is held back: before a flush, so a stream's event goes
// when its handler sends it, and when the answer ends.
func (h *emailHider) flushHeld() {
	if h.body != bodyScanned {
		return
	}
	if out := h.b.drain(); len(out) > 0 {
		_, _ = h.w.Write(out)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
