package business

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/microcosm-cc/bluemonday"
)

// The caps on a message: everything in it is a stranger's, so each is
// bounded before it is read.
const (
	// MaxMessage is a message's size (RFC822.SIZE): over it only its
	// envelope is taken, and the task says so.
	MaxMessage = 10 << 20
	// MaxSubject is a subject's runes: the task's name.
	MaxSubject = 200
	// MaxText is the text body's bytes kept in the task's description.
	MaxText = 64 << 10
	// MaxHeader is the header block's bytes; a message whose headers run past
	// it is taken by its envelope.
	MaxHeader = 64 << 10
	// MaxDepth and MaxParts bound the MIME tree walked.
	MaxDepth = 5
	MaxParts = 50
	// MaxFiles is the attachments kept a message, MaxFile each one's bytes.
	MaxFiles = 5
	MaxFile  = 10 << 20
)

// Mail is a message as the door takes it: its key, sender, subject, plain
// text and files. Text is never HTML: an HTML body is turned to text here.
type Mail struct {
	Key      []byte
	From     string
	FromName string
	Subject  string
	Text     string
	// Cut is the text cut at MaxText; Dropped the files over the caps.
	Cut      bool
	Dropped  int
	TooLarge bool
	Files    []File
}

// File is an attachment, bytes and all (at most MaxFile).
type File struct {
	Name string
	Type string
	Data []byte
}

// MessageKey is the key a message is taken once by: sha256 of its
// Message-ID (trimmed, case folded), or with none, of its sender, date,
// subject and the body's own hash. 32 bytes, never the text itself.
func MessageKey(messageID, from string, date time.Time, subject string, body []byte) []byte {
	if id := strings.ToLower(strings.TrimSpace(messageID)); id != "" {
		s := sha256.Sum256([]byte("id\x00" + id))
		return s[:]
	}
	b := sha256.Sum256(body)
	s := sha256.Sum256([]byte(fmt.Sprintf("fb\x00%s\x00%d\x00%s\x00%x", strings.ToLower(strings.TrimSpace(from)), date.Unix(), subject, b)))
	return s[:]
}

var words = &mime.WordDecoder{CharsetReader: charsetReader}

// Parse reads a fetched message under the caps. It never fails: what can't be
// read is left out, and a message too large or with headers past MaxHeader is
// taken by its envelope.
func Parse(f Fetched) Mail {
	// A body past MaxMessage is too large whatever size the server declared
	// (a server can lie: CR-409).
	big := f.Raw == nil || f.Size > MaxMessage || len(f.Raw) > MaxMessage
	if big || headerEnd(f.Raw) < 0 {
		m := Mail{TooLarge: big, Subject: clean(f.Head.Subject, MaxSubject)}
		m.From, m.FromName = address(f.Head.From)
		m.Key = MessageKey(f.Head.MessageID, m.From, f.Head.Date, m.Subject, nil)
		return m
	}
	msg, err := mail.ReadMessage(bytes.NewReader(f.Raw))
	if err != nil {
		return Mail{Key: MessageKey("", "", time.Time{}, "", f.Raw)}
	}
	subject, err := words.DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		subject = msg.Header.Get("Subject")
	}
	m := Mail{Subject: clean(subject, MaxSubject)}
	m.From, m.FromName = address(msg.Header.Get("From"))
	date, _ := msg.Header.Date()
	var w walk
	w.part(msg.Header.Get("Content-Type"), msg.Header.Get("Content-Transfer-Encoding"), "", msg.Body, 0, &m)
	text := w.plain
	if text == "" && w.html != "" {
		text = helpers.HTMLToText(bluemonday.UGCPolicy().Sanitize(w.html))
	}
	text = strings.TrimSpace(strings.ToValidUTF8(text, "�"))
	if len(text) > MaxText {
		cut := MaxText
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text, m.Cut = text[:cut], true
	}
	m.Text = text
	m.Key = MessageKey(msg.Header.Get("Message-Id"), m.From, date, m.Subject, []byte(w.plain+w.html))
	return m
}

// headerEnd is where a message's header block ends, -1 past MaxHeader.
func headerEnd(raw []byte) int {
	head := raw[:min(len(raw), MaxHeader+4)]
	if i := bytes.Index(head, []byte("\r\n\r\n")); i >= 0 {
		return i
	}
	if i := bytes.Index(head, []byte("\n\n")); i >= 0 {
		return i
	}
	return -1
}

// address is a From header's address (lower-cased) and name.
func address(h string) (string, string) {
	p := mail.AddressParser{WordDecoder: words}
	a, err := p.Parse(h)
	if err != nil {
		return "", ""
	}
	return strings.ToLower(clean(a.Address, 320)), clean(a.Name, 200)
}

// clean is s on one line with no control characters, at most n runes.
func clean(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n])
	}
	return s
}

// walk gathers a message's first plain and HTML bodies.
type walk struct {
	plain, html string
	parts       int
}

func (w *walk) part(contentType, encoding, disposition string, body io.Reader, depth int, m *Mail) {
	w.parts++
	if w.parts > MaxParts || depth > MaxDepth {
		return
	}
	media, params, err := mime.ParseMediaType(contentType)
	if err != nil || media == "" {
		media, params = "text/plain", map[string]string{}
	}
	if strings.HasPrefix(media, "multipart/") {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			p, err := mr.NextRawPart()
			if err != nil {
				return
			}
			w.part(p.Header.Get("Content-Type"), p.Header.Get("Content-Transfer-Encoding"), p.Header.Get("Content-Disposition"), p, depth+1, m)
		}
	}
	disp, dparams, _ := mime.ParseMediaType(disposition)
	name := dparams["filename"]
	if name == "" {
		name = params["name"]
	}
	if disp == "attachment" || name != "" || !strings.HasPrefix(media, "text/") {
		if len(m.Files) >= MaxFiles {
			m.Dropped++
			return
		}
		data, err := io.ReadAll(io.LimitReader(decoded(encoding, body), MaxFile+1))
		if err != nil || len(data) > MaxFile || len(data) == 0 {
			m.Dropped++
			return
		}
		if name == "" {
			name = "attachment"
		}
		m.Files = append(m.Files, File{Name: clean(name, 200), Type: media, Data: data})
		return
	}
	if (media == "text/plain" && w.plain != "") || (media == "text/html" && w.html != "") {
		return
	}
	data, _ := io.ReadAll(io.LimitReader(decoded(encoding, body), MaxMessage))
	text := latin(params["charset"], data)
	switch media {
	case "text/html":
		w.html = text
	default:
		w.plain = text
	}
}

// decoded is a part's body with its transfer encoding undone.
func decoded(encoding string, r io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, r)
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	}
	return r
}

// latin is data as UTF-8: Latin-1 decoded, anything else taken as UTF-8
// (bytes that aren't are replaced when the text is kept).
func latin(charset string, data []byte) string {
	switch strings.ToLower(charset) {
	case "iso-8859-1", "latin1", "windows-1252", "us-ascii":
		r := make([]rune, len(data))
		for i, b := range data {
			r[i] = rune(b)
		}
		return string(r)
	}
	return string(data)
}

var errCharset = errors.New("triage mail: charset not read")

// charsetReader decodes a header word's charset: UTF-8 and Latin-1 only.
func charsetReader(charset string, in io.Reader) (io.Reader, error) {
	switch strings.ToLower(charset) {
	case "utf-8", "us-ascii":
		return in, nil
	case "iso-8859-1", "latin1", "windows-1252":
		data, err := io.ReadAll(io.LimitReader(in, MaxHeader))
		if err != nil {
			return nil, err
		}
		return strings.NewReader(latin(charset, data)), nil
	}
	return nil, errCharset
}
