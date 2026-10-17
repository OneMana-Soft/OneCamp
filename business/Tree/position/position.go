// Package position orders siblings: fractional keys that sort as strings,
// and placing one sibling among others with them. Pure; business/Tree and
// business/Places use it, and so does anything that adds to a tree.
package position

// Positions: where a node sits among its siblings, as a string that sorts.
//
// A node's place is a key such that siblings sorted by their keys, byte by
// byte, are in the order people put them. A move writes one key, the moved
// node's, chosen between its new neighbours' keys, so nothing else is
// rewritten and two people reordering different rows never touch the same
// node. Keys are base 62 ("0-9A-Za-z", in ASCII order, so the graph's string
// sort agrees) and come in two parts: an integer part whose first character
// says how long it is, and a fraction. Appending after the last key bumps the
// integer ("a0", "a1", … "az", "b00"), so a list built by adding at the end
// grows its keys by a character every few thousand rows rather than every row;
// a key between two others extends the fraction ("a0" < "a0V" < "a1").
//
// This is David Greenspan's fractional indexing ("Implementing Fractional
// Indexing", 2020), the scheme Figma and Linear use, written for this repo.

import (
	"errors"
	"strings"
)

const digits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// smallestInteger is "A" followed by 26 zeros: the one integer part below
// which nothing can be made, so no key may be it.
var smallestInteger = "A" + strings.Repeat("0", 26)

// ErrBadPosition is a key that isn't one this scheme makes, or a pair of keys
// in the wrong order. The caller numbers the siblings afresh (Spread).
var ErrBadPosition = errors.New("position: not a position key, or keys out of order")

// ValidPosition says whether s is a key this scheme could have made.
func ValidPosition(s string) bool {
	if s == "" || s == smallestInteger {
		return false
	}
	n, ok := integerLength(s[0])
	if !ok || n > len(s) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if strings.IndexByte(digits, s[i]) < 0 {
			return false
		}
	}
	return s[len(s)-1] != '0' || len(s) == n
}

// integerLength is how long the integer part is, from its first character:
// "a" to "z" make 2 to 27 characters, "Z" to "A" 2 to 27 for the negatives.
func integerLength(head byte) (int, bool) {
	switch {
	case head >= 'a' && head <= 'z':
		return int(head-'a') + 2, true
	case head >= 'A' && head <= 'Z':
		return int('Z'-head) + 2, true
	}
	return 0, false
}

func integerPart(key string) string {
	n, _ := integerLength(key[0])
	return key[:n]
}

// Between is a key after a and before b. An empty a is the start of the list
// and an empty b its end, so Between("", "") is the first key of an empty
// list, Between(last, "") a key after the last, and Between("", first) one
// before the first.
func Between(a, b string) (string, error) {
	if (a != "" && !ValidPosition(a)) || (b != "" && !ValidPosition(b)) {
		return "", ErrBadPosition
	}
	if a != "" && b != "" && a >= b {
		return "", ErrBadPosition
	}
	if a == "" {
		if b == "" {
			return "a0", nil
		}
		ib := integerPart(b)
		fb := b[len(ib):]
		if ib == smallestInteger {
			m, err := midpoint("", fb)
			return ib + m, err
		}
		if ib < b {
			return ib, nil
		}
		res, ok := decrementInteger(ib)
		if !ok {
			return "", ErrBadPosition
		}
		return res, nil
	}
	if b == "" {
		ia := integerPart(a)
		fa := a[len(ia):]
		if i, ok := incrementInteger(ia); ok {
			return i, nil
		}
		m, err := midpoint(fa, "")
		return ia + m, err
	}
	ia, ib := integerPart(a), integerPart(b)
	fa, fb := a[len(ia):], b[len(ib):]
	if ia == ib {
		m, err := midpoint(fa, fb)
		return ia + m, err
	}
	i, ok := incrementInteger(ia)
	if !ok {
		return "", ErrBadPosition
	}
	if i < b {
		return i, nil
	}
	m, err := midpoint(fa, "")
	return ia + m, err
}

// Spread is n keys in order, all after a and before b (either may be empty,
// as in Between): for numbering a run of siblings afresh.
func Spread(a, b string, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	if n == 1 {
		k, err := Between(a, b)
		return []string{k}, err
	}
	if b == "" {
		out := make([]string, 0, n)
		c := a
		for i := 0; i < n; i++ {
			k, err := Between(c, "")
			if err != nil {
				return nil, err
			}
			out = append(out, k)
			c = k
		}
		return out, nil
	}
	if a == "" {
		out := make([]string, n)
		c := b
		for i := n - 1; i >= 0; i-- {
			k, err := Between("", c)
			if err != nil {
				return nil, err
			}
			out[i] = k
			c = k
		}
		return out, nil
	}
	mid := n / 2
	c, err := Between(a, b)
	if err != nil {
		return nil, err
	}
	left, err := Spread(a, c, mid)
	if err != nil {
		return nil, err
	}
	right, err := Spread(c, b, n-mid-1)
	if err != nil {
		return nil, err
	}
	return append(append(left, c), right...), nil
}

// midpoint is a fraction between a and b (b empty: no upper bound), neither
// ending in a zero, a < b.
func midpoint(a, b string) (string, error) {
	if b != "" && a >= b {
		return "", ErrBadPosition
	}
	if (a != "" && a[len(a)-1] == '0') || (b != "" && b[len(b)-1] == '0') {
		return "", ErrBadPosition
	}
	if b != "" {
		// Drop the common prefix, reading a missing digit of a as zero.
		n := 0
		for n < len(b) {
			ac := byte('0')
			if n < len(a) {
				ac = a[n]
			}
			if ac != b[n] {
				break
			}
			n++
		}
		if n > 0 {
			rest := ""
			if n < len(a) {
				rest = a[n:]
			}
			m, err := midpoint(rest, b[n:])
			return b[:n] + m, err
		}
	}
	digitA := 0
	if a != "" {
		digitA = strings.IndexByte(digits, a[0])
	}
	digitB := len(digits)
	if b != "" {
		digitB = strings.IndexByte(digits, b[0])
	}
	if digitB-digitA > 1 {
		return string(digits[(digitA+digitB+1)/2]), nil
	}
	// The first digits are next to each other.
	if len(b) > 1 {
		return b[:1], nil
	}
	rest := ""
	if len(a) > 1 {
		rest = a[1:]
	}
	m, err := midpoint(rest, "")
	return string(digits[digitA]) + m, err
}

func incrementInteger(x string) (string, bool) {
	head, digs := x[0], []byte(x[1:])
	carry := true
	for i := len(digs) - 1; carry && i >= 0; i-- {
		d := strings.IndexByte(digits, digs[i]) + 1
		if d == len(digits) {
			digs[i] = '0'
		} else {
			digs[i] = digits[d]
			carry = false
		}
	}
	if !carry {
		return string(head) + string(digs), true
	}
	if head == 'Z' {
		return "a0", true
	}
	if head == 'z' {
		return "", false
	}
	h := head + 1
	if h > 'a' {
		digs = append(digs, '0')
	} else {
		digs = digs[:len(digs)-1]
	}
	return string(h) + string(digs), true
}

func decrementInteger(x string) (string, bool) {
	head, digs := x[0], []byte(x[1:])
	borrow := true
	for i := len(digs) - 1; borrow && i >= 0; i-- {
		d := strings.IndexByte(digits, digs[i]) - 1
		if d == -1 {
			digs[i] = digits[len(digits)-1]
		} else {
			digs[i] = digits[d]
			borrow = false
		}
	}
	if !borrow {
		return string(head) + string(digs), true
	}
	if head == 'a' {
		return "Z" + string(digits[len(digits)-1]), true
	}
	if head == 'A' {
		return "", false
	}
	h := head - 1
	if h < 'Z' {
		digs = append(digs, digits[len(digits)-1])
	} else {
		digs = digs[:len(digs)-1]
	}
	return string(h) + string(digs), true
}
