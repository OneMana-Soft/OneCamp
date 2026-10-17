package businness

// A doc's page chrome (docs programme B6): its look, the docs that link to it,
// and a doc in the trash as its page shows it.
//
// THE LOOK (D-2, D-7). An icon is content its people choose: an emoji, or one
// of OneCamp's glyphs ("glyph:rocket", or "glyph:rocket:sky" in a camp
// colour). A cover is one of the gallery's ("gallery:dusk") or an image
// uploaded to the doc, kept as "<doc uuid>/<file uuid>" so that a copy's cover
// still names the file's own doc, as a copy's images do. Full width, the font
// and small text are the page's defaults for everyone; the web app keeps a
// person's own full width on top (D-7). Who may change it is the Access
// Resolver's to say (edit, so a hard lock holds an editor off it), first.

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/akashc777/OneCamp/business/Access/level"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// Look is a change to a doc's look; a nil field is left as it is, and an empty
// icon, cover or font takes it away.
type Look struct {
	Icon        *string `json:"icon,omitempty"`
	Cover       *string `json:"cover,omitempty"`
	CoverOffset *int    `json:"cover_offset,omitempty"`
	FullWidth   *bool   `json:"full_width,omitempty"`
	Font        *string `json:"font,omitempty"`
	SmallText   *bool   `json:"small_text,omitempty"`
}

// ErrBadLook is a look that can't be kept; its message says why, to the
// person who asked.
type ErrBadLook struct{ Why string }

func (e ErrBadLook) Error() string { return e.Why }

var (
	glyphIcon    = regexp.MustCompile(`^glyph:[a-z][a-z0-9-]{0,31}(:(gray|sun|moss|lake|sky|dusk|berry))?$`)
	galleryCover = regexp.MustCompile(`^gallery:[a-z][a-z0-9-]{0,23}$`)
)

// emojiIcon says whether s is one emoji, as a picker gives it: a short run of
// symbols with what joins and modifies them (skin tones, the joiner,
// presentation selectors, keycaps and flags' tags). Words aren't icons. Pure.
func emojiIcon(s string) bool {
	if s == "" || len(s) > 32 || !utf8.ValidString(s) || utf8.RuneCountInString(s) > 12 {
		return false
	}
	symbol, keycap := false, strings.ContainsRune(s, 0x20E3)
	for _, r := range s {
		switch {
		case unicode.Is(unicode.So, r):
			symbol = true
		case r >= 0x1F1E6 && r <= 0x1F1FF: // regional indicators: a flag's two halves
			symbol = true
		case unicode.Is(unicode.Sk, r), r == 0x200D, r == 0xFE0E, r == 0xFE0F, r == 0x20E3:
		case r >= 0xE0020 && r <= 0xE007F: // tags: a subdivision's flag
		case keycap && (r == '#' || r == '*' || (r >= '0' && r <= '9')):
			symbol = true
		default:
			return false
		}
	}
	return symbol
}

// Check says why a change can't be kept on the doc, or nil. An upload's cover
// must be the doc's own: an existing copy's cover is only ever kept, never
// set again. Pure.
func (l Look) Check(docUUID string) error {
	if l.Icon == nil && l.Cover == nil && l.CoverOffset == nil && l.FullWidth == nil && l.Font == nil && l.SmallText == nil {
		return ErrBadLook{"Say what to change: the icon, the cover, the width, the font or the text size."}
	}
	if l.Icon != nil && *l.Icon != "" && !glyphIcon.MatchString(*l.Icon) && !emojiIcon(*l.Icon) {
		return ErrBadLook{"An icon is one emoji or one of OneCamp's icons."}
	}
	if l.Cover != nil && *l.Cover != "" && !galleryCover.MatchString(*l.Cover) {
		doc, file, ok := strings.Cut(*l.Cover, "/")
		if !ok || !strings.EqualFold(doc, docUUID) || uuid.Validate(file) != nil {
			return ErrBadLook{"A cover is one from the gallery or an image uploaded to this doc."}
		}
	}
	if l.CoverOffset != nil && (*l.CoverOffset < 0 || *l.CoverOffset > 100) {
		return ErrBadLook{"A cover's position is 0 to 100."}
	}
	if l.Font != nil {
		switch *l.Font {
		case "", "serif", "mono":
		default:
			return ErrBadLook{"The font is the default, serif or mono."}
		}
	}
	return nil
}

// CoverFile is the file an uploaded cover names, when the change sets one:
// the caller checks it is one of the doc's files. Pure.
func (l Look) CoverFile() (string, bool) {
	if l.Cover == nil || *l.Cover == "" || galleryCover.MatchString(*l.Cover) {
		return "", false
	}
	_, file, ok := strings.Cut(*l.Cover, "/")
	return strings.ToLower(file), ok
}

// changed names what a change sets, for its event. Pure.
func (l Look) changed() string {
	var out []string
	for _, f := range []struct {
		name string
		set  bool
	}{{"icon", l.Icon != nil}, {"cover", l.Cover != nil || l.CoverOffset != nil}, {"full_width", l.FullWidth != nil}, {"font", l.Font != nil}, {"small_text", l.SmallText != nil}} {
		if f.set {
			out = append(out, f.name)
		}
	}
	return strings.Join(out, ",")
}

// SetLook changes a live doc's look, checked first (Check), and tells the
// events bus. ErrNotFound / ErrDocDeleted when there's no live doc.
func SetLook(ctx context.Context, docUUID string, l Look, by Actor) error {
	if err := l.Check(docUUID); err != nil {
		return err
	}
	lower := func(s *string) *string {
		if s == nil {
			return nil
		}
		v := *s
		if !glyphIcon.MatchString(v) && !galleryCover.MatchString(v) && !emojiIcon(v) {
			v = strings.ToLower(v)
		}
		return &v
	}
	err := domain.WriteDocLook(ctx, docUUID, domain.LookWrite{
		Icon: l.Icon, Cover: lower(l.Cover), CoverOffset: l.CoverOffset, FullWidth: l.FullWidth, Font: l.Font, SmallText: l.SmallText,
	})
	switch {
	case errors.Is(err, domain.ErrDocDeleted):
		return ErrDocDeleted
	case errors.Is(err, domain.ErrNotFound):
		return ErrNotFound
	case err != nil:
		return err
	}
	resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
		Kind: resourceEventBusiness.DocRestyled, ResourceKind: "doc", ResourceUUID: docUUID, Actor: by, At: time.Now(),
		Data: map[string]string{"changed": l.changed()},
	})
	return nil
}

// BacklinkCap bounds the docs a doc's "Linked from" names; at the cap the
// page says "200+".
const BacklinkCap = 200

// Mentioners are the live docs that link to a doc, newest edit first: for the
// caller to keep the ones the person may open (I12).
func Mentioners(ctx context.Context, docUUID string) ([]*dgraphStruct.DgraphDoc, error) {
	return domain.GetDocMentioners(ctx, docUUID, BacklinkCap)
}

// TrashedPage is a doc in the trash (or archived) as its page shows it to
// someone who could open it before: what it said and looked like, who
// deleted it and when, how many pages went with it, and what they may do.
type TrashedPage struct {
	UUID        string     `json:"doc_uuid"`
	Title       string     `json:"doc_title"`
	Body        string     `json:"doc_body"`
	Icon        string     `json:"doc_icon,omitempty"`
	Cover       string     `json:"doc_cover_key,omitempty"`
	CoverOffset *int       `json:"doc_cover_offset,omitempty"`
	FullWidth   bool       `json:"doc_full_width,omitempty"`
	Font        string     `json:"doc_font,omitempty"`
	SmallText   bool       `json:"doc_small_text,omitempty"`
	DeletedAt   time.Time  `json:"deleted_at"`
	DeletedBy   *Person    `json:"deleted_by,omitempty"`
	Archived    bool       `json:"archived,omitempty"`
	Subpages    int        `json:"subpages"`
	CanRestore  bool       `json:"can_restore"`
	CanPurge    bool       `json:"can_purge"`
	PurgeAt     *time.Time `json:"purge_at,omitempty"`
}

// ErrStillLive is a trash page asked for a doc that isn't in the trash.
var ErrStillLive = errors.New("the doc isn't in the trash")

// TrashedPageOf is a deleted doc's page for the person: ErrNotFound when
// there's no such doc or they couldn't open it before it was deleted (they're
// told nothing of it), ErrStillLive when it isn't deleted.
func TrashedPageOf(ctx context.Context, docUUID string, v Viewer, isAdmin bool) (*TrashedPage, error) {
	doc, err := domain.GetDocForTrashPage(ctx, docUUID, v.UID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d := DecideDoc(doc, v)
	switch {
	case d.Was < level.View:
		return nil, ErrNotFound
	case !d.Deleted || doc.DeletedAt == nil:
		return nil, ErrStillLive
	}
	p := &TrashedPage{
		UUID: doc.Uuid, Title: doc.Title, Body: doc.Body, Icon: doc.Icon, Cover: doc.CoverKey, CoverOffset: doc.CoverOffset,
		FullWidth: doc.FullWidth != nil && *doc.FullWidth, Font: doc.Font, SmallText: doc.SmallText != nil && *doc.SmallText,
		DeletedAt: *doc.DeletedAt, DeletedBy: PersonOf(doc.DeletedBy), Archived: d.Archived,
		CanRestore: CanRestore(doc, v, isAdmin), CanPurge: CanPurge(doc, v, isAdmin),
	}
	if doc.DeletedBatch != "" {
		sizes, err := domain.CountDocBatches(ctx, []string{doc.DeletedBatch})
		if err != nil {
			return nil, err
		}
		p.Subpages = max(sizes[doc.DeletedBatch]-1, 0)
		if !d.Archived {
			at := doc.DeletedAt.Add(trashRetention())
			p.PurgeAt = &at
		}
	}
	return p, nil
}
