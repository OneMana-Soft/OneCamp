package formula

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

type tokKind uint8

const (
	tEOF tokKind = iota
	tNum
	tStr
	tField
	tIdent
	tOp
	tLParen
	tRParen
	tComma
)

type token struct {
	kind tokKind
	text string
	num  float64
	// pos and end are the rune offsets of the token's first character and
	// the one after its last.
	pos, end int
}

// SyntaxError is a formula that can't be read, and where (a 1-based column).
type SyntaxError struct {
	Pos int
	Msg string
}

func (e *SyntaxError) Error() string {
	if e.Pos > 0 {
		return fmt.Sprintf("%s (at character %d)", e.Msg, e.Pos)
	}
	return e.Msg
}

// Message is what went wrong without where: for a stored formula, whose
// positions count its fields by id rather than the names people see.
func Message(err error) string {
	var se *SyntaxError
	if errors.As(err, &se) {
		return se.Msg
	}
	return err.Error()
}

func syntaxErr(pos int, format string, args ...interface{}) *SyntaxError {
	return &SyntaxError{Pos: pos + 1, Msg: fmt.Sprintf(format, args...)}
}

// lex splits a formula into tokens. Positions are rune offsets, so an error
// points at the character people see.
func lex(src string) ([]token, error) {
	rs := []rune(src)
	var out []token
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case unicode.IsSpace(r):
			i++
		case unicode.IsDigit(r) || (r == '.' && i+1 < len(rs) && unicode.IsDigit(rs[i+1])):
			start := i
			for i < len(rs) && (unicode.IsDigit(rs[i]) || rs[i] == '.') {
				i++
			}
			if i < len(rs) && (rs[i] == 'e' || rs[i] == 'E') {
				j := i + 1
				if j < len(rs) && (rs[j] == '+' || rs[j] == '-') {
					j++
				}
				if j < len(rs) && unicode.IsDigit(rs[j]) {
					for j < len(rs) && unicode.IsDigit(rs[j]) {
						j++
					}
					i = j
				}
			}
			text := string(rs[start:i])
			f, err := strconv.ParseFloat(text, 64)
			if err != nil {
				return nil, syntaxErr(start, "%q isn't a number", clip(text))
			}
			out = append(out, token{kind: tNum, num: f, text: text, pos: start, end: i})
		case r == '"' || r == '\'':
			start := i
			s, next, ok := quoted(rs, i, r)
			if !ok {
				return nil, syntaxErr(start, "This text has no closing %c", r)
			}
			out = append(out, token{kind: tStr, text: s, pos: start, end: next})
			i = next
		case r == '{':
			start := i
			s, next, ok := quoted(rs, i, '}')
			if !ok {
				return nil, syntaxErr(start, "A field name has no closing }")
			}
			name := strings.TrimSpace(s)
			if name == "" {
				return nil, syntaxErr(start, "{} needs a field's name inside it")
			}
			out = append(out, token{kind: tField, text: name, pos: start, end: next})
			i = next
		case unicode.IsLetter(r) || r == '_':
			start := i
			for i < len(rs) && (unicode.IsLetter(rs[i]) || unicode.IsDigit(rs[i]) || rs[i] == '_') {
				i++
			}
			out = append(out, token{kind: tIdent, text: string(rs[start:i]), pos: start, end: i})
		case r == '(':
			out = append(out, token{kind: tLParen, text: "(", pos: i, end: i + 1})
			i++
		case r == ')':
			out = append(out, token{kind: tRParen, text: ")", pos: i, end: i + 1})
			i++
		case r == ',':
			out = append(out, token{kind: tComma, text: ",", pos: i, end: i + 1})
			i++
		default:
			op := ""
			if i+1 < len(rs) {
				switch two := string(rs[i : i+2]); two {
				case "!=", "<>", "<=", ">=":
					op = two
				}
			}
			if op == "" && strings.ContainsRune("+-*/&=<>", r) {
				op = string(r)
			}
			if op == "" {
				return nil, syntaxErr(i, "%q can't be used here", string(r))
			}
			out = append(out, token{kind: tOp, text: op, pos: i, end: i + len([]rune(op))})
			i += len([]rune(op))
		}
	}
	return append(out, token{kind: tEOF, pos: len(rs), end: len(rs)}), nil
}

// quoted reads from the opening character at rs[i] up to close, with a
// backslash escaping the next character. It returns the text, the index after
// the closing character, and false when there is none.
func quoted(rs []rune, i int, close rune) (string, int, bool) {
	var b strings.Builder
	for j := i + 1; j < len(rs); j++ {
		switch rs[j] {
		case '\\':
			if j+1 < len(rs) {
				j++
				if rs[j] == 'n' {
					b.WriteRune('\n')
				} else {
					b.WriteRune(rs[j])
				}
			}
		case close:
			return b.String(), j + 1, true
		default:
			b.WriteRune(rs[j])
		}
	}
	return "", len(rs), false
}
