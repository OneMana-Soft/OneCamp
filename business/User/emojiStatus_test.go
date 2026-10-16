package business

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	adapterUser "github.com/akashc777/OneCamp/adapter/User"
)

// The one rule for a status, which the web's menu keeps too: an emoji from
// the picker, 1 to 100 characters of words, and a "Clear after" the server
// can count or a time still to come. Nothing was checked: an empty emoji or
// empty words were kept, and the web then drew no status at all.
func TestAStatusIsCheckedByOneRule(t *testing.T) {
	// Wednesday 15 Oct 2026, 12:00 UTC.
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	ok := func(change func(*adapterUser.UserEmojiStatusInput)) *adapterUser.UserEmojiStatusInput {
		in := &adapterUser.UserEmojiStatusInput{EmojiUuid: "sandwich", StatusDesc: "Lunch", ExpiryTimeIn: "30m", TimeZone: "Asia/Kolkata"}
		if change != nil {
			change(in)
		}
		return in
	}

	t.Run("keeps a status within the rule, its words trimmed", func(t *testing.T) {
		for name, in := range map[string]*adapterUser.UserEmojiStatusInput{
			"30 minutes":              ok(nil),
			"a ZWJ emoji's id":        ok(func(in *adapterUser.UserEmojiStatusInput) { in.EmojiUuid = "female-technologist" }),
			"a flag":                  ok(func(in *adapterUser.UserEmojiStatusInput) { in.EmojiUuid = "flag-in" }),
			"+1":                      ok(func(in *adapterUser.UserEmojiStatusInput) { in.EmojiUuid = "+1" }),
			"100 characters of emoji": ok(func(in *adapterUser.UserEmojiStatusInput) { in.StatusDesc = strings.Repeat("🍕", 100) }),
			"a time still to come": ok(func(in *adapterUser.UserEmojiStatusInput) {
				in.ExpiryTimeIn, in.ExpiryTimeAt = "custom", strconv.FormatInt(now.Add(time.Hour).Unix(), 10)
			}),
			"no time zone (UTC)":       ok(func(in *adapterUser.UserEmojiStatusInput) { in.TimeZone = "" }),
			"an old zone name (alias)": ok(func(in *adapterUser.UserEmojiStatusInput) { in.TimeZone = "Asia/Calcutta" }),
		} {
			got, err := CheckEmojiStatus(in, now)
			if err != nil {
				t.Errorf("%s: refused (%v)", name, err)
				continue
			}
			if got.EmojiDesc != strings.TrimSpace(in.StatusDesc) || got.ExpiryAt == nil || !got.ExpiryAt.After(now) {
				t.Errorf("%s: kept as %+v", name, got)
			}
		}
		got, _ := CheckEmojiStatus(ok(func(in *adapterUser.UserEmojiStatusInput) { in.StatusDesc = "  Lunch  " }), now)
		if got.EmojiDesc != "Lunch" {
			t.Errorf("words kept as %q; want them trimmed", got.EmojiDesc)
		}
	})

	t.Run("refuses what it can't keep, saying why", func(t *testing.T) {
		for name, c := range map[string]struct {
			in   *adapterUser.UserEmojiStatusInput
			says string
		}{
			"nothing":                  {nil, "Write a few words for your status."},
			"no emoji":                 {ok(func(in *adapterUser.UserEmojiStatusInput) { in.EmojiUuid = " " }), "Choose an emoji for your status."},
			"an emoji's glyph, not id": {ok(func(in *adapterUser.UserEmojiStatusInput) { in.EmojiUuid = "🍕" }), "Choose an emoji for your status."},
			"no words":                 {ok(func(in *adapterUser.UserEmojiStatusInput) { in.StatusDesc = "   " }), "Write a few words for your status."},
			"101 characters":           {ok(func(in *adapterUser.UserEmojiStatusInput) { in.StatusDesc = strings.Repeat("a", 101) }), "Keep your status to 100 characters."},
			"an unknown Clear after":   {ok(func(in *adapterUser.UserEmojiStatusInput) { in.ExpiryTimeIn = "forever" }), "Choose when your status clears."},
			"custom with no time":      {ok(func(in *adapterUser.UserEmojiStatusInput) { in.ExpiryTimeIn = "custom" }), "Choose when your status clears."},
			"a time that isn't one":    {ok(func(in *adapterUser.UserEmojiStatusInput) { in.ExpiryTimeIn, in.ExpiryTimeAt = "custom", "NaN" }), "Choose when your status clears."},
			"a time that has passed": {ok(func(in *adapterUser.UserEmojiStatusInput) {
				in.ExpiryTimeIn, in.ExpiryTimeAt = "custom", strconv.FormatInt(now.Add(-time.Minute).Unix(), 10)
			}), "Choose a time that hasn't passed."},
			"an unknown time zone": {ok(func(in *adapterUser.UserEmojiStatusInput) { in.TimeZone = "Mars/Olympus" }), "Your time zone isn't one the server knows, so your status can't clear on time."},
		} {
			_, err := CheckEmojiStatus(c.in, now)
			var refused *StatusRefused
			if !errors.As(err, &refused) || refused.Error() != c.says {
				t.Errorf("%s: %v; want refused with %q", name, err, c.says)
			}
		}
	})

	t.Run("counts Today and This week where the person is", func(t *testing.T) {
		got, err := CheckEmojiStatus(ok(func(in *adapterUser.UserEmojiStatusInput) { in.ExpiryTimeIn = "today" }), now)
		if err != nil {
			t.Fatal(err)
		}
		// 17:30 in Kolkata; the day ends at 23:59:59 there.
		if want := time.Date(2026, 10, 15, 23, 59, 59, 0, time.FixedZone("IST", 5*3600+1800)); !got.ExpiryAt.Equal(want) {
			t.Errorf("Today clears at %v; want %v", got.ExpiryAt, want)
		}
	})
}
