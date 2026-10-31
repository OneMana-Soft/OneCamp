package formula

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	// SET_TIMEZONE reads any zone, whether or not the server's image has
	// the zone files.
	_ "time/tzdata"
)

// dateCost is the fixed work of a date function: reading a moment on the
// reader's clock, and its parts, takes about 1.5 µs (YEAR(NOW()) was
// charged 3 steps for it before tables B2 measured).
const dateCost = 30

func init() {
	register(map[string]*function{
		"HOUR":    {min: 1, max: 1, result: KindNumber, cost: dateCost, eager: datePart("HOUR", func(t time.Time) int { return t.Hour() })},
		"MINUTE":  {min: 1, max: 1, result: KindNumber, cost: dateCost, eager: datePart("MINUTE", func(t time.Time) int { return t.Minute() })},
		"SECOND":  {min: 1, max: 1, result: KindNumber, cost: dateCost, eager: datePart("SECOND", func(t time.Time) int { return t.Second() })},
		"WEEKNUM": {min: 1, max: 2, result: KindNumber, cost: dateCost, eager: fnWeeknum},
		"DATESTR": {min: 1, max: 1, result: KindText, cost: dateCost, eager: dateText("DATESTR", "YYYY-MM-DD")},
		"TIMESTR": {min: 1, max: 1, result: KindText, cost: dateCost, eager: dateText("TIMESTR", "HH:mm:ss")},

		"DATETIME_FORMAT": {min: 1, max: 2, result: KindText, cost: dateCost, eager: fnDatetimeFormat},
		"DATETIME_PARSE":  {min: 1, max: 3, result: KindDate, cost: dateCost, eager: fnDatetimeParse},
		"SET_TIMEZONE":    {min: 2, max: 2, result: KindDate, cost: dateCost, eager: fnSetTimezone},
		"SET_LOCALE":      {min: 2, max: 2, result: KindDate, cost: dateCost, eager: fnSetLocale},

		"WORKDAY":   {min: 2, max: 3, result: KindDate, cost: dateCost, eager: fnWorkday},
		"IS_SAME":   {min: 2, max: 3, result: KindBool, cost: dateCost, eager: fnIsSame},
		"IS_BEFORE": {min: 2, max: 2, result: KindBool, cost: dateCost, eager: datesCompared("IS_BEFORE", func(c int) bool { return c < 0 })},
		"IS_AFTER":  {min: 2, max: 2, result: KindBool, cost: dateCost, eager: datesCompared("IS_AFTER", func(c int) bool { return c > 0 })},
		"TONOW":     {min: 1, max: 1, result: KindNumber, cost: dateCost, eager: daysFromNow("TONOW")},
		"FROMNOW":   {min: 1, max: 1, result: KindNumber, cost: dateCost, eager: daysFromNow("FROMNOW")},
	})
}

// dateText writes a date in a fixed layout (DATESTR, TIMESTR).
func dateText(name, layout string) func(*env, []Value) Value {
	return func(e *env, a []Value) Value {
		if a[0].Kind == KindBlank {
			return Blank()
		}
		d, bad, ok := date(e, name, a[0])
		if !ok {
			return bad
		}
		return Text(formatMoment(calendar(e, d), layout, localeOf(d.Locale)))
	}
}

// WEEKNUM(date, [start]) is the week of the year, the week with 1 January
// being the first, starting on Sunday or, when start says so, Monday.
func fnWeeknum(e *env, a []Value) Value {
	if a[0].Kind == KindBlank {
		return Blank()
	}
	d, bad, ok := date(e, "WEEKNUM", a[0])
	if !ok {
		return bad
	}
	start := time.Sunday
	if len(a) == 2 {
		switch strings.ToLower(strings.TrimSpace(a[1].Text())) {
		case "sunday", "":
		case "monday":
			start = time.Monday
		default:
			return Error(fmt.Sprintf("WEEKNUM's week starts on \"Sunday\" or \"Monday\", not %q", clip(a[1].Text())))
		}
	}
	t := calendar(e, d)
	jan1 := time.Date(t.Year(), time.January, 1, 0, 0, 0, 0, time.UTC)
	offset := (int(jan1.Weekday()) - int(start) + 7) % 7
	return Number(float64((t.YearDay()-1+offset)/7 + 1))
}

// DATETIME_FORMAT(date, [format]) writes a date with Moment's tokens
// ("D MMMM YYYY", "HH:mm"), as Airtable's does; ISO 8601 when no format is
// given. A moment is on the reader's clock, or SET_TIMEZONE's.
func fnDatetimeFormat(e *env, a []Value) Value {
	if a[0].Kind == KindBlank {
		return Blank()
	}
	d, bad, ok := date(e, "DATETIME_FORMAT", a[0])
	if !ok {
		return bad
	}
	layout := "YYYY-MM-DDTHH:mm:ssZ"
	if d.Day {
		layout = "YYYY-MM-DD"
	}
	if len(a) == 2 && a[1].Kind != KindBlank {
		layout = a[1].Text()
		if len(layout) > maxReadLen*2 {
			return Error("DATETIME_FORMAT's format is too long")
		}
	}
	// Each character of a layout costs about 50 ns to write out ("LLLL"
	// writes a dozen tokens).
	e.spend(20 * len(layout))
	return Text(formatMoment(calendar(e, d), layout, localeOf(d.Locale)))
}

// DATETIME_PARSE(text, [format], [locale]) reads a date written in a format
// of Moment's tokens; without one, as a date cell is read (2026-10-08, ISO
// 8601) or in a few common ways ("8 October 2026", "October 8, 2026").
func fnDatetimeParse(e *env, a []Value) Value {
	if a[0].Kind == KindBlank {
		return Blank()
	}
	if a[0].Kind == KindDate {
		return a[0]
	}
	s := strings.TrimSpace(a[0].Text())
	if len(s) > maxReadLen {
		return Error("DATETIME_PARSE can't read text that long as a date")
	}
	loc := localeOf("")
	if len(a) == 3 && a[2].Kind != KindBlank {
		l, ok := locales[strings.ToLower(strings.TrimSpace(a[2].Text()))]
		if !ok {
			return Error(fmt.Sprintf("DATETIME_PARSE doesn't know the locale %q", clip(a[2].Text())))
		}
		loc = l
	}
	// Each layout tried reads the text a token at a time.
	try := func() { e.spend(16 * len(s)) }
	if len(a) >= 2 && a[1].Kind != KindBlank {
		layout := a[1].Text()
		if len(layout) > maxReadLen*2 {
			return Error("DATETIME_PARSE's format is too long")
		}
		try()
		if v, ok := parseMoment(s, layout, loc, e.loc); ok {
			return v
		}
		return Error(fmt.Sprintf("%q isn't a date written %q", clip(s), clip(layout)))
	}
	if v, ok := ParseDate(s); ok {
		return v
	}
	for _, layout := range []string{"D MMMM YYYY", "MMMM D, YYYY", "D MMM YYYY", "MMM D, YYYY", "YYYY/MM/DD", "D MMMM YYYY HH:mm", "YYYY-MM-DD HH:mm:ss"} {
		try()
		if v, ok := parseMoment(s, layout, loc, e.loc); ok {
			return v
		}
	}
	return Error(fmt.Sprintf("%q isn't a date DATETIME_PARSE can read; give it a format", clip(s)))
}

// maxZone is the longest zone name read ("America/Argentina/ComodRivadavia").
const maxZone = 40

// SET_TIMEZONE(date, zone) has DATETIME_FORMAT and the date's parts read a
// moment on that zone's clock ("Asia/Kolkata"), rather than the reader's.
func fnSetTimezone(e *env, a []Value) Value {
	if a[0].Kind == KindBlank {
		return Blank()
	}
	d, bad, ok := date(e, "SET_TIMEZONE", a[0])
	if !ok {
		return bad
	}
	name := strings.TrimSpace(a[1].Text())
	if name == "" || len(name) > maxZone || strings.Contains(name, "..") {
		return Error(fmt.Sprintf("SET_TIMEZONE doesn't know the zone %q", clip(name)))
	}
	zone, err := loadZone(name)
	if err != nil {
		return Error(fmt.Sprintf("SET_TIMEZONE doesn't know the zone %q", clip(name)))
	}
	if d.Day {
		return d // a day is the same day everywhere
	}
	d.Time, d.Zoned = d.Time.In(zone), true
	return d
}

// zones keeps the zones SET_TIMEZONE has loaded: loading one reads and
// parses its rules, about 20 µs.
var zones = struct {
	sync.Mutex
	m map[string]*time.Location
}{m: map[string]*time.Location{}}

func loadZone(name string) (*time.Location, error) {
	zones.Lock()
	defer zones.Unlock()
	if z, ok := zones.m[name]; ok {
		return z, nil
	}
	z, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	if len(zones.m) >= 600 { // about as many as there are
		zones.m = map[string]*time.Location{}
	}
	zones.m[name] = z
	return z, nil
}

// SET_LOCALE(date, locale) has DATETIME_FORMAT write the date's month and
// day names in that language ("fr", "de").
func fnSetLocale(e *env, a []Value) Value {
	if a[0].Kind == KindBlank {
		return Blank()
	}
	d, bad, ok := date(e, "SET_LOCALE", a[0])
	if !ok {
		return bad
	}
	code := strings.ToLower(strings.TrimSpace(a[1].Text()))
	if _, ok := locales[code]; !ok {
		return Error(fmt.Sprintf("SET_LOCALE doesn't know the locale %q; it knows %s", clip(code), localeNames()))
	}
	d.Locale = code
	return d
}

// maxHolidays is the most holidays WORKDAY and WORKDAY_DIFF read.
const maxHolidays = 1000

// holidays reads the days a holidays value names: a list of dates, or text
// with dates split by commas ("2026-12-25, 2026-12-26").
func holidays(e *env, name string, v Value) (map[time.Time]bool, Value, bool) {
	out := map[time.Time]bool{}
	items := v.items()
	if v.list == nil && v.Kind == KindText {
		items = nil
		for _, part := range strings.Split(v.Str, ",") {
			if part = strings.TrimSpace(part); part != "" {
				items = append(items, Text(part))
			}
		}
	}
	if len(items) > maxHolidays {
		return nil, Error(fmt.Sprintf("%s reads at most %d holidays", name, maxHolidays)), false
	}
	// Reading a date from text takes about 3 µs.
	e.spend(60 * len(items))
	for _, it := range items {
		d, ok := asDate(it)
		if !ok {
			return nil, Error(fmt.Sprintf("%s's holidays are dates, and %q isn't one", name, clip(it.Text()))), false
		}
		out[dayOf(calendar(e, e.local(d)))] = true
	}
	return out, Value{}, true
}

// maxWorkdays is the furthest WORKDAY counts, in weekdays (about 400 years).
const maxWorkdays = 100000

// WORKDAY(start, days, [holidays]) is the weekday that many weekdays after
// start (before it when negative), skipping Saturdays, Sundays and the
// holidays, as Airtable's does.
func fnWorkday(e *env, a []Value) Value {
	if a[0].Kind == KindBlank {
		return Blank()
	}
	s, bad, ok := date(e, "WORKDAY", a[0])
	if !ok {
		return bad
	}
	n, bad, ok := num("WORKDAY", a[1])
	if !ok {
		return bad
	}
	if math.Abs(n) > maxWorkdays {
		return Error(fmt.Sprintf("WORKDAY counts at most %d weekdays", maxWorkdays))
	}
	off := map[time.Time]bool{}
	if len(a) == 3 && a[2].Kind != KindBlank {
		if off, bad, ok = holidays(e, "WORKDAY", a[2]); !ok {
			return bad
		}
	}
	day, left, step := dayOf(calendar(e, s)), int(math.Abs(math.Trunc(n))), 1
	if n < 0 {
		step = -1
	}
	// Whole weeks at once, when no holiday can fall in them.
	if len(off) == 0 && left > 10 {
		weeks := (left - 5) / 5
		day = day.AddDate(0, 0, step*weeks*7)
		left -= weeks * 5
	}
	// About 200 ns a day, weekends included, and 300 with holidays to look
	// up.
	perDay := 4
	if len(off) > 0 {
		perDay = 6
	}
	e.spend(perDay * (left + left/5*2 + 2))
	for left > 0 {
		day = day.AddDate(0, 0, step)
		if wd := day.Weekday(); wd != time.Saturday && wd != time.Sunday && !off[day] {
			left--
		}
	}
	return Date(day, true)
}

// isSameUnit reads IS_SAME's unit: year, month, week, day, hour, minute or
// second.
func isSameUnit(v Value) (string, bool) {
	u := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(v.Text())), "s")
	switch u {
	case "year", "month", "week", "day", "hour", "minute", "second":
		return u, true
	}
	return "", false
}

// IS_SAME(a, b, [unit]) is whether two dates fall in the same unit of time
// (the same day, month…), or are the same moment when no unit is given.
func fnIsSame(e *env, a []Value) Value {
	if a[0].Kind == KindBlank || a[1].Kind == KindBlank {
		return Bool(false)
	}
	x, bad, ok := date(e, "IS_SAME", a[0])
	if !ok {
		return bad
	}
	y, bad, ok := date(e, "IS_SAME", a[1])
	if !ok {
		return bad
	}
	if len(a) < 3 {
		return Bool(compareTimes(x, y) == 0)
	}
	u, ok := isSameUnit(a[2])
	if !ok {
		return Error(fmt.Sprintf("IS_SAME compares by year, month, week, day, hour, minute or second, not %q", clip(a[2].Text())))
	}
	s, t := calendar(e, x), calendar(e, y)
	same := s.Year() == t.Year()
	switch u {
	case "month":
		same = same && s.Month() == t.Month()
	case "week":
		sy, sw := s.ISOWeek()
		ty, tw := t.ISOWeek()
		same = sy == ty && sw == tw
	case "day":
		same = same && s.YearDay() == t.YearDay()
	case "hour":
		same = same && s.YearDay() == t.YearDay() && s.Hour() == t.Hour()
	case "minute":
		same = same && s.YearDay() == t.YearDay() && s.Hour() == t.Hour() && s.Minute() == t.Minute()
	case "second":
		same = same && s.YearDay() == t.YearDay() && s.Hour() == t.Hour() && s.Minute() == t.Minute() && s.Second() == t.Second()
	}
	return Bool(same)
}

func datesCompared(name string, holds func(int) bool) func(*env, []Value) Value {
	return func(e *env, a []Value) Value {
		if a[0].Kind == KindBlank || a[1].Kind == KindBlank {
			return Bool(false)
		}
		x, bad, ok := date(e, name, a[0])
		if !ok {
			return bad
		}
		y, bad, ok := date(e, name, a[1])
		if !ok {
			return bad
		}
		return Bool(holds(compareTimes(x, y)))
	}
}

// TONOW(date) and FROMNOW(date) are the whole days between the date and
// today (now, for a moment), however they're ordered, as Airtable's are.
func daysFromNow(name string) func(*env, []Value) Value {
	return func(e *env, a []Value) Value {
		if a[0].Kind == KindBlank {
			return Blank()
		}
		d, bad, ok := date(e, name, a[0])
		if !ok {
			return bad
		}
		// A day from today (the reader's), a moment from now.
		from := e.local(Date(e.now, false))
		if d.Day {
			from = Date(e.now.In(e.loc), true)
		}
		return Number(math.Abs(math.Trunc(daysBetween(from, d))))
	}
}

// locale is the words a language writes dates with.
type locale struct {
	months, shortMonths, days, shortDays [12]string
	// shortDate is how Moment's "L" writes a day in the language.
	shortDate string
	// ordinal writes 1st, 2nd… ("Do").
	ordinal func(n int) string
}

func localeOf(code string) *locale {
	if l, ok := locales[code]; ok {
		return l
	}
	return locales["en"]
}

func localeNames() string {
	return "en, en-gb, fr, de, es, it, pt, nl, sv, da, nb, pl and hi"
}

func days7(d ...string) [12]string {
	var out [12]string
	copy(out[:], d)
	return out
}

func dotOrdinal(n int) string { return strconv.Itoa(n) + "." }

func enOrdinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return strconv.Itoa(n) + suffix
}

var englishLocale = &locale{
	months:      [12]string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"},
	shortMonths: [12]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"},
	days:        days7("Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"),
	shortDays:   days7("Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"),
	shortDate:   "MM/DD/YYYY",
	ordinal:     enOrdinal,
}

// locales are the languages SET_LOCALE and DATETIME_PARSE know.
var locales = map[string]*locale{
	"en": englishLocale,
	"en-gb": {
		months: englishLocale.months, shortMonths: englishLocale.shortMonths, days: englishLocale.days, shortDays: englishLocale.shortDays,
		shortDate: "DD/MM/YYYY", ordinal: enOrdinal,
	},
	"fr": {
		months:      [12]string{"janvier", "février", "mars", "avril", "mai", "juin", "juillet", "août", "septembre", "octobre", "novembre", "décembre"},
		shortMonths: [12]string{"janv.", "févr.", "mars", "avr.", "mai", "juin", "juil.", "août", "sept.", "oct.", "nov.", "déc."},
		days:        days7("dimanche", "lundi", "mardi", "mercredi", "jeudi", "vendredi", "samedi"),
		shortDays:   days7("dim.", "lun.", "mar.", "mer.", "jeu.", "ven.", "sam."),
		shortDate:   "DD/MM/YYYY",
		ordinal: func(n int) string {
			if n == 1 {
				return "1er"
			}
			return strconv.Itoa(n)
		},
	},
	"de": {
		months:      [12]string{"Januar", "Februar", "März", "April", "Mai", "Juni", "Juli", "August", "September", "Oktober", "November", "Dezember"},
		shortMonths: [12]string{"Jan.", "Feb.", "März", "Apr.", "Mai", "Juni", "Juli", "Aug.", "Sept.", "Okt.", "Nov.", "Dez."},
		days:        days7("Sonntag", "Montag", "Dienstag", "Mittwoch", "Donnerstag", "Freitag", "Samstag"),
		shortDays:   days7("So.", "Mo.", "Di.", "Mi.", "Do.", "Fr.", "Sa."),
		shortDate:   "DD.MM.YYYY",
		ordinal:     dotOrdinal,
	},
	"es": {
		months:      [12]string{"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre"},
		shortMonths: [12]string{"ene.", "feb.", "mar.", "abr.", "may.", "jun.", "jul.", "ago.", "sept.", "oct.", "nov.", "dic."},
		days:        days7("domingo", "lunes", "martes", "miércoles", "jueves", "viernes", "sábado"),
		shortDays:   days7("dom.", "lun.", "mar.", "mié.", "jue.", "vie.", "sáb."),
		shortDate:   "DD/MM/YYYY",
		ordinal:     func(n int) string { return strconv.Itoa(n) + "º" },
	},
	"it": {
		months:      [12]string{"gennaio", "febbraio", "marzo", "aprile", "maggio", "giugno", "luglio", "agosto", "settembre", "ottobre", "novembre", "dicembre"},
		shortMonths: [12]string{"gen", "feb", "mar", "apr", "mag", "giu", "lug", "ago", "set", "ott", "nov", "dic"},
		days:        days7("domenica", "lunedì", "martedì", "mercoledì", "giovedì", "venerdì", "sabato"),
		shortDays:   days7("dom", "lun", "mar", "mer", "gio", "ven", "sab"),
		shortDate:   "DD/MM/YYYY",
		ordinal:     func(n int) string { return strconv.Itoa(n) + "º" },
	},
	"pt": {
		months:      [12]string{"janeiro", "fevereiro", "março", "abril", "maio", "junho", "julho", "agosto", "setembro", "outubro", "novembro", "dezembro"},
		shortMonths: [12]string{"jan", "fev", "mar", "abr", "mai", "jun", "jul", "ago", "set", "out", "nov", "dez"},
		days:        days7("domingo", "segunda-feira", "terça-feira", "quarta-feira", "quinta-feira", "sexta-feira", "sábado"),
		shortDays:   days7("dom", "seg", "ter", "qua", "qui", "sex", "sáb"),
		shortDate:   "DD/MM/YYYY",
		ordinal:     func(n int) string { return strconv.Itoa(n) + "º" },
	},
	"nl": {
		months:      [12]string{"januari", "februari", "maart", "april", "mei", "juni", "juli", "augustus", "september", "oktober", "november", "december"},
		shortMonths: [12]string{"jan.", "feb.", "mrt.", "apr.", "mei", "jun.", "jul.", "aug.", "sep.", "okt.", "nov.", "dec."},
		days:        days7("zondag", "maandag", "dinsdag", "woensdag", "donderdag", "vrijdag", "zaterdag"),
		shortDays:   days7("zo.", "ma.", "di.", "wo.", "do.", "vr.", "za."),
		shortDate:   "DD-MM-YYYY",
		ordinal:     func(n int) string { return strconv.Itoa(n) + "e" },
	},
	"sv": {
		months:      [12]string{"januari", "februari", "mars", "april", "maj", "juni", "juli", "augusti", "september", "oktober", "november", "december"},
		shortMonths: [12]string{"jan", "feb", "mars", "apr", "maj", "juni", "juli", "aug", "sep", "okt", "nov", "dec"},
		days:        days7("söndag", "måndag", "tisdag", "onsdag", "torsdag", "fredag", "lördag"),
		shortDays:   days7("sön", "mån", "tis", "ons", "tors", "fre", "lör"),
		shortDate:   "YYYY-MM-DD",
		ordinal:     func(n int) string { return strconv.Itoa(n) + ":e" },
	},
	"da": {
		months:      [12]string{"januar", "februar", "marts", "april", "maj", "juni", "juli", "august", "september", "oktober", "november", "december"},
		shortMonths: [12]string{"jan.", "feb.", "mar.", "apr.", "maj", "jun.", "jul.", "aug.", "sep.", "okt.", "nov.", "dec."},
		days:        days7("søndag", "mandag", "tirsdag", "onsdag", "torsdag", "fredag", "lørdag"),
		shortDays:   days7("søn.", "man.", "tirs.", "ons.", "tors.", "fre.", "lør."),
		shortDate:   "DD.MM.YYYY",
		ordinal:     dotOrdinal,
	},
	"nb": {
		months:      [12]string{"januar", "februar", "mars", "april", "mai", "juni", "juli", "august", "september", "oktober", "november", "desember"},
		shortMonths: [12]string{"jan.", "feb.", "mars", "apr.", "mai", "juni", "juli", "aug.", "sep.", "okt.", "nov.", "des."},
		days:        days7("søndag", "mandag", "tirsdag", "onsdag", "torsdag", "fredag", "lørdag"),
		shortDays:   days7("søn.", "man.", "tir.", "ons.", "tor.", "fre.", "lør."),
		shortDate:   "DD.MM.YYYY",
		ordinal:     dotOrdinal,
	},
	"pl": {
		months:      [12]string{"stycznia", "lutego", "marca", "kwietnia", "maja", "czerwca", "lipca", "sierpnia", "września", "października", "listopada", "grudnia"},
		shortMonths: [12]string{"sty", "lut", "mar", "kwi", "maj", "cze", "lip", "sie", "wrz", "paź", "lis", "gru"},
		days:        days7("niedziela", "poniedziałek", "wtorek", "środa", "czwartek", "piątek", "sobota"),
		shortDays:   days7("ndz", "pon", "wt", "śr", "czw", "pt", "sob"),
		shortDate:   "DD.MM.YYYY",
		ordinal:     dotOrdinal,
	},
	"hi": {
		months:      [12]string{"जनवरी", "फ़रवरी", "मार्च", "अप्रैल", "मई", "जून", "जुलाई", "अगस्त", "सितंबर", "अक्टूबर", "नवंबर", "दिसंबर"},
		shortMonths: [12]string{"जन॰", "फ़र॰", "मार्च", "अप्रैल", "मई", "जून", "जुल॰", "अग॰", "सित॰", "अक्टू॰", "नव॰", "दिस॰"},
		days:        days7("रविवार", "सोमवार", "मंगलवार", "बुधवार", "गुरुवार", "शुक्रवार", "शनिवार"),
		shortDays:   days7("रवि", "सोम", "मंगल", "बुध", "गुरु", "शुक्र", "शनि"),
		shortDate:   "DD/MM/YYYY",
		ordinal:     strconv.Itoa,
	},
}

// momentTokens are Moment's format tokens, longest first, so "MMMM" is read
// before "MM".
var momentTokens = []string{
	"YYYY", "MMMM", "dddd", "SSS", "MMM", "ddd", "DDDD", "YY", "MM", "DD", "Do", "HH", "hh", "mm", "ss", "WW", "LLLL", "LLL", "LL", "LTS", "LT",
	"M", "D", "d", "H", "h", "m", "s", "A", "a", "Q", "W", "X", "x", "Z", "L",
}

// nextToken is the token at the start of s, or "" when s starts with a
// literal character.
func nextToken(s string) string {
	for _, t := range momentTokens {
		if strings.HasPrefix(s, t) {
			return t
		}
	}
	return ""
}

func pad2(n int) string {
	if n < 10 && n >= 0 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// formatMoment writes t with Moment's tokens; text in [brackets] is kept as
// written.
func formatMoment(t time.Time, layout string, l *locale) string {
	var b strings.Builder
	for len(layout) > 0 {
		if layout[0] == '[' {
			if end := strings.IndexByte(layout, ']'); end > 0 {
				b.WriteString(layout[1:end])
				layout = layout[end+1:]
				continue
			}
		}
		tok := nextToken(layout)
		if tok == "" {
			b.WriteByte(layout[0])
			layout = layout[1:]
			continue
		}
		layout = layout[len(tok):]
		h12 := t.Hour() % 12
		if h12 == 0 {
			h12 = 12
		}
		switch tok {
		case "YYYY":
			b.WriteString(fmt.Sprintf("%04d", t.Year()))
		case "YY":
			b.WriteString(pad2(t.Year() % 100))
		case "Q":
			b.WriteString(strconv.Itoa((int(t.Month())-1)/3 + 1))
		case "MMMM":
			b.WriteString(l.months[t.Month()-1])
		case "MMM":
			b.WriteString(l.shortMonths[t.Month()-1])
		case "MM":
			b.WriteString(pad2(int(t.Month())))
		case "M":
			b.WriteString(strconv.Itoa(int(t.Month())))
		case "DDDD":
			b.WriteString(fmt.Sprintf("%03d", t.YearDay()))
		case "DD":
			b.WriteString(pad2(t.Day()))
		case "D":
			b.WriteString(strconv.Itoa(t.Day()))
		case "Do":
			b.WriteString(l.ordinal(t.Day()))
		case "dddd":
			b.WriteString(l.days[t.Weekday()])
		case "ddd":
			b.WriteString(l.shortDays[t.Weekday()])
		case "d":
			b.WriteString(strconv.Itoa(int(t.Weekday())))
		case "WW", "W":
			_, w := t.ISOWeek()
			if tok == "WW" {
				b.WriteString(pad2(w))
			} else {
				b.WriteString(strconv.Itoa(w))
			}
		case "HH":
			b.WriteString(pad2(t.Hour()))
		case "H":
			b.WriteString(strconv.Itoa(t.Hour()))
		case "hh":
			b.WriteString(pad2(h12))
		case "h":
			b.WriteString(strconv.Itoa(h12))
		case "mm":
			b.WriteString(pad2(t.Minute()))
		case "m":
			b.WriteString(strconv.Itoa(t.Minute()))
		case "ss":
			b.WriteString(pad2(t.Second()))
		case "s":
			b.WriteString(strconv.Itoa(t.Second()))
		case "SSS":
			b.WriteString(fmt.Sprintf("%03d", t.Nanosecond()/1e6))
		case "A", "a":
			ampm := "AM"
			if t.Hour() >= 12 {
				ampm = "PM"
			}
			if tok == "a" {
				ampm = strings.ToLower(ampm)
			}
			b.WriteString(ampm)
		case "X":
			b.WriteString(strconv.FormatInt(t.Unix(), 10))
		case "x":
			b.WriteString(strconv.FormatInt(t.UnixMilli(), 10))
		case "Z":
			b.WriteString(t.Format("-07:00"))
		case "L":
			b.WriteString(formatMoment(t, l.shortDate, l))
		case "LL":
			b.WriteString(formatMoment(t, "D MMMM YYYY", l))
		case "LLL":
			b.WriteString(formatMoment(t, "D MMMM YYYY HH:mm", l))
		case "LLLL":
			b.WriteString(formatMoment(t, "dddd D MMMM YYYY HH:mm", l))
		case "LT":
			b.WriteString(formatMoment(t, "HH:mm", l))
		case "LTS":
			b.WriteString(formatMoment(t, "HH:mm:ss", l))
		}
	}
	return b.String()
}

// parseMoment reads s written in layout's tokens, in locale l; a moment with
// no zone in it is on zone's clock. A layout with no hour gives a day.
func parseMoment(s, layout string, l *locale, zone *time.Location) (Value, bool) {
	year, month, day, hour, minute, second, milli := 1970, 1, 1, 0, 0, 0, 0
	pm, ampm, timed, zoned := false, false, false, false
	var offset int
	var unix *time.Time
	number := func(min, max int) (int, bool) {
		n := 0
		for n < max && n < len(s) && s[n] >= '0' && s[n] <= '9' {
			n++
		}
		if n < min {
			return 0, false
		}
		v, _ := strconv.Atoi(s[:n])
		s = s[n:]
		return v, true
	}
	word := func(words [12]string, count int) (int, bool) {
		best, at := -1, 0
		for i := 0; i < count; i++ {
			w := words[i]
			if w != "" && len(w) > at && len(s) >= len(w) && strings.EqualFold(s[:len(w)], w) {
				best, at = i, len(w)
			}
		}
		if best < 0 {
			return 0, false
		}
		s = s[at:]
		return best, true
	}
	for len(layout) > 0 {
		if layout[0] == '[' {
			if end := strings.IndexByte(layout, ']'); end > 0 {
				lit := layout[1:end]
				if !strings.HasPrefix(s, lit) {
					return Value{}, false
				}
				s, layout = s[len(lit):], layout[end+1:]
				continue
			}
		}
		tok := nextToken(layout)
		if tok == "" {
			if len(s) == 0 || s[0] != layout[0] {
				return Value{}, false
			}
			s, layout = s[1:], layout[1:]
			continue
		}
		layout = layout[len(tok):]
		var ok bool
		switch tok {
		case "YYYY":
			year, ok = number(4, 4)
		case "YY":
			year, ok = number(2, 2)
			year += 2000
			if year > 2068 {
				year -= 100
			}
		case "MMMM", "MMM":
			var m int
			if m, ok = word(l.months, 12); !ok {
				m, ok = word(l.shortMonths, 12)
			}
			month = m + 1
		case "MM", "M":
			month, ok = number(len(tok), 2)
		case "DD", "D":
			day, ok = number(len(tok), 2)
		case "Do":
			if day, ok = number(1, 2); ok {
				for len(s) > 0 && (s[0] < '0' || s[0] > '9') && s[0] != ' ' && s[0] != ',' {
					s = s[1:]
				}
			}
		case "dddd", "ddd":
			if _, ok = word(l.days, 7); !ok {
				_, ok = word(l.shortDays, 7)
			}
		case "HH", "H", "hh", "h":
			hour, ok = number(len(tok), 2)
			timed = true
		case "mm", "m":
			minute, ok = number(len(tok), 2)
			timed = true
		case "ss", "s":
			second, ok = number(len(tok), 2)
		case "SSS":
			milli, ok = number(3, 3)
		case "A", "a":
			ampm = true
			switch {
			case len(s) >= 2 && strings.EqualFold(s[:2], "pm"):
				pm, ok = true, true
			case len(s) >= 2 && strings.EqualFold(s[:2], "am"):
				ok = true
			}
			if ok {
				s = s[2:]
			}
		case "X", "x":
			var n int
			if n, ok = number(1, 13); ok {
				t := time.Unix(int64(n), 0).UTC()
				if tok == "x" {
					t = time.UnixMilli(int64(n)).UTC()
				}
				unix = &t
			}
		case "Z":
			if s == "Z" || strings.HasPrefix(s, "Z") {
				s, ok, zoned = s[1:], true, true
				break
			}
			if len(s) >= 6 && (s[0] == '+' || s[0] == '-') && s[3] == ':' {
				h, e1 := strconv.Atoi(s[1:3])
				m, e2 := strconv.Atoi(s[4:6])
				if e1 == nil && e2 == nil {
					offset = (h*60 + m) * 60
					if s[0] == '-' {
						offset = -offset
					}
					s, ok, zoned = s[6:], true, true
				}
			}
		default:
			return Value{}, false // a token that only writes (Q, W, L…)
		}
		if !ok {
			return Value{}, false
		}
	}
	if strings.TrimSpace(s) != "" {
		return Value{}, false
	}
	if unix != nil {
		return Date(*unix, false), true
	}
	if ampm {
		if hour < 1 || hour > 12 {
			return Value{}, false
		}
		hour %= 12
		if pm {
			hour += 12
		}
	}
	if month < 1 || month > 12 || day < 1 || hour > 23 || minute > 59 || second > 59 {
		return Value{}, false
	}
	at := zone
	if zoned {
		at = time.FixedZone("", offset)
	}
	t := time.Date(year, time.Month(month), day, hour, minute, second, milli*1e6, at)
	if t.Day() != day {
		return Value{}, false // 31 February
	}
	if !timed && !zoned {
		return Date(t, true), true
	}
	return Date(t.UTC(), false), true
}
