package helpers

// The layouts people read times and dates in, wherever the server writes one
// into words (design v3: a 24-hour clock everywhere, the day before the
// month). QA SH-08 (14 Oct 2026): the briefing said "Wed 6:45 PM" beside an
// agenda saying 18:45, and the daily note "due today at 8:30 PM".
const (
	// ClockLayout is a time of day: "18:45".
	ClockLayout = "15:04"
	// DayLayout is a day: "Tue 14 Oct".
	DayLayout = "Mon 2 Jan"
	// DateLayout is a date without its weekday: "14 Oct".
	DateLayout = "2 Jan"
)
