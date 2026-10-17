package position

import (
	"math/rand"
	"sort"
	"testing"
)

func TestBetweenKnownKeys(t *testing.T) {
	for _, c := range []struct{ a, b, want string }{
		{"", "", "a0"},
		{"", "a0", "Zz"},
		{"", "Zz", "Zy"},
		{"a0", "", "a1"},
		{"a1", "", "a2"},
		{"az", "", "b00"},
		{"b0z", "", "b10"},
		{"a0", "a1", "a0V"},
		{"a1", "a2", "a1V"},
		{"a0V", "a1", "a0l"},
		{"Zz", "a0", "ZzV"},
		{"Zz", "a1", "a0"},
		{"", "Y00", "Xzzz"},
		{"bzz", "", "c000"},
		{"a0", "a0V", "a0G"},
		{"a0", "a0G", "a08"},
		{"b125", "b129", "b127"},
		{"a4", "a5", "a4V"},
		{"a0", "a01", "a00V"},
	} {
		got, err := Between(c.a, c.b)
		if err != nil || got != c.want {
			t.Errorf("Between(%q, %q) = %q, %v; want %q", c.a, c.b, got, err, c.want)
		}
	}
}

func TestBetweenRefusesWhatItCannotOrder(t *testing.T) {
	for _, c := range []struct{ a, b string }{
		{"a1", "a1"},
		{"a2", "a1"},
		{"a10", ""}, // a fraction ending in zero
		{"0", ""},   // no such head
		{"a", ""},   // too short for its head
		{smallestInteger, ""},
		{"a1", "x"},
	} {
		if got, err := Between(c.a, c.b); err == nil {
			t.Errorf("Between(%q, %q) = %q, want an error", c.a, c.b, got)
		}
	}
}

// Whatever order rows are inserted in, the keys sort into that order, stay
// valid, and grow slowly when added at the end, which is how lists are built.
func TestKeysKeepTheOrderTheyWereGivenIn(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	var list []string // in the order people see
	for i := 0; i < 2000; i++ {
		at := r.Intn(len(list) + 1)
		before, after := "", ""
		if at > 0 {
			before = list[at-1]
		}
		if at < len(list) {
			after = list[at]
		}
		k, err := Between(before, after)
		if err != nil {
			t.Fatalf("insert %d at %d between %q and %q: %v", i, at, before, after, err)
		}
		if !ValidPosition(k) {
			t.Fatalf("made an invalid key %q", k)
		}
		list = append(list[:at], append([]string{k}, list[at:]...)...)
	}
	if !sort.StringsAreSorted(list) {
		t.Fatal("keys don't sort in the order the rows were placed")
	}
	seen := map[string]bool{}
	for _, k := range list {
		if seen[k] {
			t.Fatalf("key %q given twice", k)
		}
		seen[k] = true
	}

	last := ""
	for i := 0; i < 10000; i++ {
		k, err := Between(last, "")
		if err != nil {
			t.Fatal(err)
		}
		last = k
	}
	if len(last) > 4 {
		t.Errorf("10,000 rows added at the end made a %d-character key (%q)", len(last), last)
	}
}

func TestSpreadNumbersARunInOrder(t *testing.T) {
	for _, c := range []struct {
		a, b string
		n    int
	}{{"", "", 5}, {"a0", "", 4}, {"", "a0", 4}, {"a0", "a1", 9}, {"Zz", "a0", 3}} {
		keys, err := Spread(c.a, c.b, c.n)
		if err != nil || len(keys) != c.n {
			t.Fatalf("Spread(%q, %q, %d) = %v, %v", c.a, c.b, c.n, keys, err)
		}
		all := append([]string{}, keys...)
		if c.a != "" {
			all = append([]string{c.a}, all...)
		}
		if c.b != "" {
			all = append(all, c.b)
		}
		for i := 1; i < len(all); i++ {
			if all[i-1] >= all[i] {
				t.Fatalf("Spread(%q, %q, %d) = %v: not strictly between and in order", c.a, c.b, c.n, keys)
			}
		}
	}
}
