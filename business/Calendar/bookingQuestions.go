package Calendar

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
)

// A booking page's questions (gcal-7 H3): Google keeps custom questions for
// paid plans, every plan has them here.
const (
	maxQuestions     = 10
	maxQuestionLabel = 200
	maxChoiceOptions = 10
	maxOptionLabel   = 100
	maxShortAnswer   = 200
	maxLongAnswer    = 1000
	minPhoneLength   = 6
	maxPhoneLength   = 20
	minPhoneDigits   = 6
	phoneCharacters  = "0123456789 +()-."
)

// QuestionInput is a question as a page's owner edits it: no id for a new one.
type QuestionInput struct {
	Id       string   `json:"id,omitempty"`
	Label    string   `json:"label"`
	Kind     string   `json:"kind"`
	Required bool     `json:"required"`
	Options  []string `json:"options,omitempty"`
}

// GuestAnswer is a visitor's answer to one of the page's questions.
type GuestAnswer struct {
	QuestionId string `json:"question_id"`
	Answer     string `json:"answer"`
}

// oneLine is text on one line, its runs of space made one.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// lineBreaks makes every line break a "\n": "\r\n", "\r", NEL and the line
// and paragraph separators.
var lineBreaks = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\u0085", "\n", "\u2028", "\n", "\u2029", "\n")

// hiddenRune is a character nobody sees that changes what text shows or
// breaks where it's kept: a control character other than a line break or a
// tab (a NUL, which Postgres refuses; an escape), or a bidi embedding,
// override or isolate, which can show text reversed. Pure.
func hiddenRune(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	return unicode.IsControl(r) || (r >= '\u202A' && r <= '\u202E') || (r >= '\u2066' && r <= '\u2069')
}

// refuseHidden is the sentence refusing s, what a person wrote, when it
// holds a hidden character; nil when it holds none. Pure.
func refuseHidden(s, what string) error {
	if strings.ContainsFunc(s, hiddenRune) {
		return &AvailabilityError{Msg: "Remove the hidden characters from " + what + "."}
	}
	return nil
}

// checkQuestions tidies and validates a page's questions, in its owner's
// order: a new one gets an id of its own, and one given back keeps its id
// and is in kept, for the save to check it is the page's (a new page has
// none to give back). Pure but for the new ids.
func checkQuestions(in []QuestionInput, newPage bool) (qs bookingModel.Questions, kept []string, err error) {
	if len(in) > maxQuestions {
		return nil, nil, &AvailabilityError{Msg: fmt.Sprintf("A page can ask up to %d questions.", maxQuestions)}
	}
	out := make(bookingModel.Questions, 0, len(in))
	kept = make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, q := range in {
		label := oneLine(q.Label)
		if label == "" || utf8.RuneCountInString(label) > maxQuestionLabel {
			return nil, nil, &AvailabilityError{Msg: "Give each question a label of up to 200 characters."}
		}
		if err := refuseHidden(label, "a question's label"); err != nil {
			return nil, nil, err
		}
		id := uuid.NewString()
		if q.Id != "" {
			given, err := uuid.Parse(q.Id)
			if newPage || err != nil || seen[given.String()] {
				return nil, nil, &AvailabilityError{Msg: "That question isn't one of this page's."}
			}
			id = given.String()
			kept = append(kept, id)
		}
		seen[id] = true
		checked := bookingModel.Question{Id: id, Label: label, Kind: q.Kind, Required: q.Required}
		switch q.Kind {
		case bookingModel.QuestionShort, bookingModel.QuestionLong, bookingModel.QuestionPhone:
		case bookingModel.QuestionChoice:
			options, err := checkOptions(label, q.Options)
			if err != nil {
				return nil, nil, err
			}
			checked.Options = options
		default:
			return nil, nil, &AvailabilityError{Msg: fmt.Sprintf("“%s” takes a short answer, a long one, a choice or a phone number.", label)}
		}
		out = append(out, checked)
	}
	return out, kept, nil
}

// checkOptions tidies and validates a choice's options. Pure.
func checkOptions(label string, in []string) ([]string, error) {
	if len(in) < 2 || len(in) > maxChoiceOptions {
		return nil, &AvailabilityError{Msg: fmt.Sprintf("Give “%s” 2 to %d choices.", label, maxChoiceOptions)}
	}
	out := make([]string, 0, len(in))
	for _, o := range in {
		o = oneLine(o)
		if o == "" || utf8.RuneCountInString(o) > maxOptionLabel {
			return nil, &AvailabilityError{Msg: fmt.Sprintf("Make each choice of “%s” 1 to 100 characters.", label)}
		}
		if err := refuseHidden(o, fmt.Sprintf("the choices of “%s”", label)); err != nil {
			return nil, err
		}
		if slices.Contains(out, o) {
			return nil, &AvailabilityError{Msg: fmt.Sprintf("“%s” has the choice “%s” twice.", label, o)}
		}
		out = append(out, o)
	}
	return out, nil
}

// CheckAnswers checks a visitor's answers against the page's questions as
// they are now, and keeps each answered one with its question's label, in
// the page's order: a question the page doesn't ask is refused, as is one
// answered twice, and every required one must be answered. Pure.
func CheckAnswers(qs []bookingModel.Question, in []GuestAnswer) (bookingModel.Answers, error) {
	given := make(map[string]string, len(in))
	for _, a := range in {
		_, twice := given[a.QuestionId]
		asked := slices.ContainsFunc(qs, func(q bookingModel.Question) bool { return q.Id == a.QuestionId })
		if twice || !asked {
			return nil, &AvailabilityError{Msg: "A question you answered isn't on this page any more. Reload the page and try again."}
		}
		given[a.QuestionId] = a.Answer
	}
	out := make(bookingModel.Answers, 0, len(given))
	for _, q := range qs {
		a, err := checkAnswer(q, given[q.Id])
		if err != nil {
			return nil, err
		}
		if a != "" {
			out = append(out, bookingModel.Answer{QuestionId: q.Id, Label: q.Label, Answer: a})
		}
	}
	return out, nil
}

// checkAnswer tidies and validates one answer to q: "" for one left blank
// that may be. Pure.
func checkAnswer(q bookingModel.Question, a string) (string, error) {
	if q.Kind == bookingModel.QuestionLong {
		a = strings.TrimSpace(lineBreaks.Replace(a))
	} else {
		a = oneLine(a)
	}
	if err := refuseHidden(a, fmt.Sprintf("your answer to “%s”", q.Label)); err != nil {
		return "", err
	}
	if a == "" {
		if q.Required {
			return "", &AvailabilityError{Msg: fmt.Sprintf("Answer “%s”.", q.Label)}
		}
		return "", nil
	}
	n := utf8.RuneCountInString(a)
	switch q.Kind {
	case bookingModel.QuestionLong:
		if n > maxLongAnswer {
			return "", &AvailabilityError{Msg: fmt.Sprintf("Your answer to “%s” can be up to 1,000 characters.", q.Label)}
		}
	case bookingModel.QuestionChoice:
		if !slices.Contains(q.Options, a) {
			return "", &AvailabilityError{Msg: fmt.Sprintf("Pick one of the choices for “%s”.", q.Label)}
		}
	case bookingModel.QuestionPhone:
		digits := 0
		for _, r := range a {
			if r >= '0' && r <= '9' {
				digits++
			}
		}
		other := strings.ContainsFunc(a, func(r rune) bool { return !strings.ContainsRune(phoneCharacters, r) })
		if n < minPhoneLength || n > maxPhoneLength || digits < minPhoneDigits || other {
			return "", &AvailabilityError{Msg: fmt.Sprintf("Enter a phone number for “%s”.", q.Label)}
		}
	default:
		if n > maxShortAnswer {
			return "", &AvailabilityError{Msg: fmt.Sprintf("Your answer to “%s” can be up to 200 characters.", q.Label)}
		}
	}
	return a, nil
}

// AnswerLines writes answers as "Label: answer" lines, for the owner's event
// and mail. Pure.
func AnswerLines(a []bookingModel.Answer) string {
	lines := make([]string, len(a))
	for i, x := range a {
		lines[i] = labelled(x.Label, x.Answer)
	}
	return strings.Join(lines, "\n")
}

// labelled writes "Label: text", each further line of text indented by two
// spaces: no line someone wrote reads as a label of its own. Pure.
func labelled(label, text string) string {
	return label + ": " + strings.ReplaceAll(lineBreaks.Replace(text), "\n", "\n  ")
}

// bookingDescription is the owner's event's description: who booked, then
// the guest's note and answers, each under its label. Pure.
func bookingDescription(title, name, email, note string, answers []bookingModel.Answer) string {
	desc := fmt.Sprintf("Booked through your page “%s” by %s (%s).", title, name, email)
	if note != "" {
		desc += "\n\n" + labelled("Note", note)
	}
	if lines := AnswerLines(answers); lines != "" {
		desc += "\n\n" + lines
	}
	return desc
}
