package provider

// Custom fields, in OneCamp's terms (business/TaskField). A provider says
// which fields a project has (SourceProject.Fields) and each task's values
// of them (SourceTask.Fields); the import makes the fields on the OneCamp
// project and sets the values. A provider that only learns of a field from
// its tasks (Jira, where fields belong to the whole site) can leave
// SourceProject.Fields empty: a field is made when a value first needs it.

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
)

// The field types a SourceField can be.
const (
	FieldText        = taskFieldModel.TypeText
	FieldNumber      = taskFieldModel.TypeNumber
	FieldMoney       = taskFieldModel.TypeMoney
	FieldDate        = taskFieldModel.TypeDate
	FieldSelect      = taskFieldModel.TypeSelect
	FieldMultiSelect = taskFieldModel.TypeMultiSelect
	FieldPerson      = taskFieldModel.TypePerson
	FieldCheckbox    = taskFieldModel.TypeCheckbox
	FieldURL         = taskFieldModel.TypeURL
)

// SourceField is one custom field of a source project.
type SourceField struct {
	SourceID string
	Name     string
	Type     string         // one of the Field* types
	Options  []SourceOption // select and multi-select, in the source's order
	Currency string         // money: a three-letter code, or "" for USD
}

// SourceOption is one choice of a select or multi-select field.
type SourceOption struct {
	SourceID string
	Label    string
	Color    string // a palette colour (PaletteColor), or "" for one in turn
}

// SourceFieldValue is a task's value of one field. Value holds:
//
//	text, url     string
//	number        float64
//	money         float64, in the currency's units (12.5 is 12.50)
//	date          string, "2006-01-02"
//	select        string, the option's SourceID
//	multi_select  []string, option SourceIDs
//	person        string, the user's SourceID
//	checkbox      bool
//
// Field is the whole field, so it can be made from the value alone; a field
// already made is found by its SourceID and only options it lacks are taken
// from here, so Field.Options needs at least the options the value names.
//
// Text is the value as the source shows it ("Ada, Grace", "$1,250.50"), when
// it has a way of showing it: a value that can't be kept as a field (a
// project already at its limit of fields, a link that isn't one) is written
// into the task's description with it, so nothing a task held is lost.
//
// A value whose Field has no Type is one no field can hold (a formula the
// source works out): it goes into the task's description, with its Text.
//
// InDescription says the source keeps the value in the task's description
// already (monday.com lists a people column of two there, as a field holds
// one), so it isn't listed there a second time.
type SourceFieldValue struct {
	Field         SourceField
	Value         any
	Text          string
	InDescription bool
}

// palette is OneCamp's option colours (business/TaskStatus.Colors) with the
// colour each is drawn in, for matching a source's own colours.
var palette = []struct {
	name    string
	r, g, b float64
}{
	{"slate", 0x64, 0x74, 0x8b}, {"red", 0xef, 0x44, 0x44}, {"orange", 0xf9, 0x73, 0x16},
	{"amber", 0xf5, 0x9e, 0x0b}, {"yellow", 0xea, 0xb3, 0x08}, {"lime", 0x84, 0xcc, 0x16},
	{"green", 0x22, 0xc5, 0x5e}, {"emerald", 0x10, 0xb9, 0x81}, {"teal", 0x14, 0xb8, 0xa6},
	{"cyan", 0x06, 0xb6, 0xd4}, {"sky", 0x0e, 0xa5, 0xe9}, {"blue", 0x3b, 0x82, 0xf6},
	{"indigo", 0x63, 0x66, 0xf1}, {"violet", 0x8b, 0x5c, 0xf6}, {"purple", 0xa8, 0x55, 0xf7},
	{"pink", 0xec, 0x48, 0x99}, {"rose", 0xf4, 0x3f, 0x5e},
}

// namedColors are the colour names sources use (Asana, Notion, Trello) that
// aren't already a palette name.
var namedColors = map[string]string{
	"gray": "slate", "grey": "slate", "cool-gray": "slate", "black": "slate", "default": "slate", "brown": "amber",
	"yellow-orange": "amber", "yellow-green": "lime", "blue-green": "teal", "aqua": "cyan",
	"magenta": "pink", "hot-pink": "pink",
}

// PaletteColor is the palette colour nearest a source's colour, given as
// "#rrggbb" (ClickUp, monday.com) or by name (Asana's "blue-green", Notion's
// "brown", Trello's "sky_dark"); "" when there's none to go by. Pure.
func PaletteColor(c string) string {
	c = strings.ToLower(strings.TrimSpace(c))
	if c == "" || c == "none" {
		return ""
	}
	if strings.HasPrefix(c, "#") {
		return nearestHex(c)
	}
	// Trello's shades ("green_dark", "sky_light") and Notion's backgrounds
	// ("blue_background") are their colour.
	if i := strings.IndexAny(c, "_ "); i > 0 {
		c = c[:i]
	}
	if n, ok := namedColors[c]; ok {
		c = n
	}
	for _, p := range palette {
		if p.name == c {
			return c
		}
	}
	return ""
}

// nearestHex is the palette colour closest to "#rgb" or "#rrggbb", by the
// "redmean" distance (closer to how colours look than plain RGB).
func nearestHex(c string) string {
	h := strings.TrimPrefix(c, "#")
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if len(h) != 6 {
		return ""
	}
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return ""
	}
	r, g, b := float64(v>>16&0xff), float64(v>>8&0xff), float64(v&0xff)
	// Greys (and near-black and near-white) are slate whatever their tint.
	if max(r, g, b)-min(r, g, b) < 24 {
		return "slate"
	}
	best, bestD := "", math.MaxFloat64
	for _, p := range palette {
		rm := (r + p.r) / 2
		dr, dg, db := r-p.r, g-p.g, b-p.b
		d := (2+rm/256)*dr*dr + 4*dg*dg + (2+(255-rm)/256)*db*db
		if d < bestD {
			best, bestD = p.name, d
		}
	}
	return best
}

// currencySymbols are the currency signs sources show for money columns
// that don't give a code (monday.com, Notion's number formats).
var currencySymbols = map[string]string{
	"$": "USD", "us$": "USD", "€": "EUR", "£": "GBP", "₹": "INR", "¥": "JPY", "₩": "KRW",
	"₽": "RUB", "₺": "TRY", "₪": "ILS", "₫": "VND", "₱": "PHP", "฿": "THB", "zł": "PLN",
	"kr": "SEK", "r$": "BRL", "a$": "AUD", "c$": "CAD", "chf": "CHF", "₦": "NGN",
}

// currencyCodes are the ISO 4217 codes a source's money is likely kept in.
// A code not here isn't taken for one: a unit like "hrs" or "pcs" is three
// letters too.
var currencyCodes = map[string]bool{}

func init() {
	for _, c := range strings.Fields(`USD EUR GBP INR JPY CNY AUD CAD CHF SEK NOK DKK NZD SGD HKD KRW BRL MXN
		ZAR RUB TRY PLN CZK HUF ILS AED SAR THB IDR MYR PHP VND NGN EGP PKR BDT LKR NPR KES GHS ARS CLP COP
		PEN UYU TWD UAH RON BGN ISK QAR KWD BHD OMR JOD MAD TND XOF XAF`) {
		currencyCodes[c] = true
	}
}

// CurrencyCode is a currency a source names by its code (ClickUp's
// currency_type, Asana's currency_code): any three letters, as ISO 4217
// codes are, or a sign CurrencyOf knows. "" when it's neither. Pure.
func CurrencyCode(s string) string {
	code := strings.ToUpper(strings.TrimSpace(s))
	if len(code) == 3 && strings.Trim(code, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") == "" {
		return code
	}
	return CurrencyOf(s)
}

// IDString is an id a source sends as a JSON string or a number ("123" or
// 123), as a string; "" for anything else. Pure.
func IDString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

// CurrencyOf is the three-letter code for a currency's sign or a code it
// knows ("€", "eur", "EUR"), or "" when it isn't one. A unit like "hrs" is
// three letters too, so units are matched only against known codes. Pure.
func CurrencyOf(s string) string {
	s = strings.TrimSpace(s)
	if code, ok := currencySymbols[strings.ToLower(s)]; ok {
		return code
	}
	if code := strings.ToUpper(s); currencyCodes[code] {
		return code
	}
	return ""
}
