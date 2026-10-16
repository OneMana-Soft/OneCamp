package helpers

import (
	"testing"
	"time"
)

// When a status set to clear after a length clears, counted from now where
// the person is. "This week" set on a Sunday ran on to the next Sunday.
func TestAStatusClearsAfterWhatWasChosen(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	wed := time.Date(2026, 10, 14, 10, 0, 0, 0, ist)
	sun := time.Date(2026, 10, 18, 10, 0, 0, 0, ist)
	for _, c := range []struct {
		code string
		from time.Time
		want time.Time
	}{
		{UserEmojiStatusExpiryIn30M, wed, wed.Add(30 * time.Minute)},
		{UserEmojiStatusExpiryIn1H, wed, wed.Add(time.Hour)},
		{UserEmojiStatusExpiryIn4H, wed, wed.Add(4 * time.Hour)},
		{UserEmojiStatusExpiryInToday, wed, time.Date(2026, 10, 14, 23, 59, 59, 0, ist)},
		{UserEmojiStatusExpiryInWeek, wed, time.Date(2026, 10, 18, 23, 59, 59, 0, ist)},
		{UserEmojiStatusExpiryInWeek, sun, time.Date(2026, 10, 18, 23, 59, 59, 0, ist)},
		{UserEmojiStatusExpiryInCustom, wed, time.Time{}},
	} {
		if got := GetUserEmojiExipryTime(c.code, c.from); !got.Equal(c.want) {
			t.Errorf("%s from %s: %v; want %v", c.code, c.from.Weekday(), got, c.want)
		}
	}
}
