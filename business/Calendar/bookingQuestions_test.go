package Calendar

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
)

// questionPage is a page good enough to save, asking qs.
func questionPage(qs ...QuestionInput) PageInput {
	return PageInput{Slug: "intro-call", Title: "Intro call", DurationMinutes: 30, MaxDaysAhead: 30, Active: true,
		Hours: WorkingHours{Days: []int{1, 2}, Start: "09:00", End: "17:00", TZ: "UTC"}, Questions: qs}
}

// onPage is in as an edit of a page already saved.
func onPage(in PageInput) PageInput {
	in.Id = uuid.NewString()
	return in
}

// refusedWith is whether err is a sentence for the person that says want.
func refusedWith(err error, want string) bool {
	var ae *AvailabilityError
	return errors.As(err, &ae) && strings.Contains(ae.Msg, want)
}

func TestCheckPageQuestions(t *testing.T) {
	now := time.Now()
	t.Run("a page asks nothing unless its owner asks", func(t *testing.T) {
		p, err := CheckPage(questionPage(), now)
		if err != nil || p.Questions == nil || len(p.Questions) != 0 {
			t.Fatalf("questions %#v err %v, want an empty list", p.Questions, err)
		}
	})
	t.Run("a new question gets an id, and one it's given back keeps its own", func(t *testing.T) {
		kept := uuid.NewString()
		p, err := CheckPage(onPage(questionPage(
			QuestionInput{Label: "  Your   company ", Kind: "short", Required: true},
			QuestionInput{Id: kept, Label: "Phone", Kind: "phone"},
			QuestionInput{Label: "Topic", Kind: "choice", Options: []string{" Sales ", "Support"}},
			QuestionInput{Label: "Anything else", Kind: "long", Options: []string{"dropped"}},
		)), now)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Questions) != 4 {
			t.Fatalf("got %d questions", len(p.Questions))
		}
		q := p.Questions
		if _, err := uuid.Parse(q[0].Id); err != nil || q[0].Id == q[2].Id {
			t.Errorf("a new question's id %q isn't its own", q[0].Id)
		}
		if q[1].Id != kept {
			t.Errorf("a question given back changed its id: %q, want %q", q[1].Id, kept)
		}
		if q[0].Label != "Your company" || !q[0].Required || q[0].Kind != "short" {
			t.Errorf("first question %#v", q[0])
		}
		if strings.Join(q[2].Options, "|") != "Sales|Support" {
			t.Errorf("choices %q, want them tidied", q[2].Options)
		}
		if q[3].Options != nil {
			t.Errorf("a long answer has no choices, got %q", q[3].Options)
		}
		// The order is the owner's: reordering keeps each id.
		again, err := CheckPage(onPage(questionPage(
			QuestionInput{Id: q[2].Id, Label: "Topic", Kind: "choice", Options: []string{"Sales", "Support"}},
			QuestionInput{Id: q[0].Id, Label: "Company", Kind: "short"},
		)), now)
		if err != nil || again.Questions[0].Id != q[2].Id || again.Questions[1].Id != q[0].Id {
			t.Errorf("a reorder changed the ids: %#v %v", again.Questions, err)
		}
		if strings.Join(again.KeptQuestionIds, " ") != q[2].Id+" "+q[0].Id {
			t.Errorf("the ids given back %q, want %q and %q for the save to check they're the page's", again.KeptQuestionIds, q[2].Id, q[0].Id)
		}
	})
	t.Run("a new page has no questions of its own to give back", func(t *testing.T) {
		_, err := CheckPage(questionPage(QuestionInput{Id: uuid.NewString(), Label: "Company", Kind: "short"}), now)
		if !refusedWith(err, "isn't one of this page's") {
			t.Errorf("an id given on a new page: err %v, want it refused", err)
		}
	})
	short := func(label string) QuestionInput { return QuestionInput{Label: label, Kind: "short"} }
	eleven := make([]QuestionInput, 11)
	for i := range eleven {
		eleven[i] = short("Question " + string(rune('A'+i)))
	}
	id := uuid.NewString()
	for _, c := range []struct {
		name string
		qs   []QuestionInput
		want string
	}{
		{"more than ten", eleven, "up to 10 questions"},
		{"no label", []QuestionInput{short("   ")}, "label of up to 200 characters"},
		{"a label too long", []QuestionInput{short(strings.Repeat("é", 201))}, "label of up to 200 characters"},
		{"no kind", []QuestionInput{{Label: "Company"}}, "short answer, a long one, a choice or a phone number"},
		{"an unknown kind", []QuestionInput{{Label: "Company", Kind: "date"}}, "short answer, a long one, a choice or a phone number"},
		{"one choice", []QuestionInput{{Label: "Topic", Kind: "choice", Options: []string{"Sales"}}}, "2 to 10 choices"},
		{"eleven choices", []QuestionInput{{Label: "Topic", Kind: "choice", Options: strings.Split("a b c d e f g h i j k", " ")}}, "2 to 10 choices"},
		{"an empty choice", []QuestionInput{{Label: "Topic", Kind: "choice", Options: []string{"Sales", " "}}}, "1 to 100 characters"},
		{"a choice too long", []QuestionInput{{Label: "Topic", Kind: "choice", Options: []string{"Sales", strings.Repeat("x", 101)}}}, "1 to 100 characters"},
		{"a choice twice", []QuestionInput{{Label: "Topic", Kind: "choice", Options: []string{"Sales", " Sales"}}}, "twice"},
		{"an id that isn't one", []QuestionInput{{Id: "q1", Label: "Company", Kind: "short"}}, "isn't one of this page's"},
		{"one id twice", []QuestionInput{{Id: id, Label: "A", Kind: "short"}, {Id: id, Label: "B", Kind: "short"}}, "isn't one of this page's"},
		{"a NUL in a label", []QuestionInput{short("Com\x00pany")}, "Remove the hidden characters"},
		{"a reversed run in a label", []QuestionInput{short("Com\u202Epany")}, "Remove the hidden characters"},
		{"an escape in a choice", []QuestionInput{{Label: "Topic", Kind: "choice", Options: []string{"Sales", "Sup\x1bport"}}}, "Remove the hidden characters"},
	} {
		t.Run("refuses "+c.name, func(t *testing.T) {
			if _, err := CheckPage(onPage(questionPage(c.qs...)), now); !refusedWith(err, c.want) {
				t.Errorf("err %v, want a sentence saying %q", err, c.want)
			}
		})
	}
}

func TestCheckAnswers(t *testing.T) {
	qs := []bookingModel.Question{
		{Id: "q-company", Label: "Company", Kind: "short", Required: true},
		{Id: "q-agenda", Label: "Agenda", Kind: "long"},
		{Id: "q-topic", Label: "Topic", Kind: "choice", Options: []string{"Sales", "Support"}},
		{Id: "q-phone", Label: "Phone", Kind: "phone"},
	}
	t.Run("answers are kept with their questions' labels, in the page's order", func(t *testing.T) {
		got, err := CheckAnswers(qs, []GuestAnswer{
			{QuestionId: "q-phone", Answer: " +91 (80) 1234-5678 "},
			{QuestionId: "q-company", Answer: "  Acme   Corp "},
			{QuestionId: "q-agenda", Answer: "  "},
			{QuestionId: "q-topic", Answer: "Support"},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []bookingModel.Answer{
			{QuestionId: "q-company", Label: "Company", Answer: "Acme Corp"},
			{QuestionId: "q-topic", Label: "Topic", Answer: "Support"},
			{QuestionId: "q-phone", Label: "Phone", Answer: "+91 (80) 1234-5678"},
		}
		if len(got) != len(want) {
			t.Fatalf("got %#v", got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("answer %d: %#v, want %#v", i, got[i], want[i])
			}
		}
	})
	t.Run("a page with no questions keeps an empty list", func(t *testing.T) {
		got, err := CheckAnswers(nil, nil)
		if err != nil || got == nil || len(got) != 0 {
			t.Errorf("got %#v %v", got, err)
		}
	})
	company := GuestAnswer{QuestionId: "q-company", Answer: "Acme"}
	for _, c := range []struct {
		name string
		in   []GuestAnswer
		want string
	}{
		{"a required one unanswered", nil, "Answer “Company”."},
		{"a required one blank", []GuestAnswer{{QuestionId: "q-company", Answer: "  "}}, "Answer “Company”."},
		{"a question the page doesn't ask", []GuestAnswer{company, {QuestionId: "q-gone", Answer: "x"}}, "isn't on this page any more"},
		{"a question answered twice", []GuestAnswer{company, company}, "isn't on this page any more"},
		{"a short answer too long", []GuestAnswer{{QuestionId: "q-company", Answer: strings.Repeat("é", 201)}}, "“Company” can be up to 200 characters"},
		{"a long answer too long", []GuestAnswer{company, {QuestionId: "q-agenda", Answer: strings.Repeat("x", 1001)}}, "“Agenda” can be up to 1,000 characters"},
		{"a NUL", []GuestAnswer{{QuestionId: "q-company", Answer: "Ac\x00me"}}, "Remove the hidden characters from your answer to “Company”."},
		{"an escape", []GuestAnswer{{QuestionId: "q-company", Answer: "Ac\x1b[31mme"}}, "Remove the hidden characters from your answer to “Company”."},
		{"a reversed run", []GuestAnswer{{QuestionId: "q-company", Answer: "Ac\u202Eme"}}, "Remove the hidden characters from your answer to “Company”."},
		{"an isolate in a long answer", []GuestAnswer{company, {QuestionId: "q-agenda", Answer: "line one\n\u2066two\u2069"}}, "Remove the hidden characters from your answer to “Agenda”."},
		{"a NUL in a long answer", []GuestAnswer{company, {QuestionId: "q-agenda", Answer: "a\x00b"}}, "Remove the hidden characters from your answer to “Agenda”."},
		{"a choice it doesn't offer", []GuestAnswer{company, {QuestionId: "q-topic", Answer: "Billing"}}, "one of the choices for “Topic”"},
		{"a phone with letters", []GuestAnswer{company, {QuestionId: "q-phone", Answer: "call me 123456"}}, "phone number for “Phone”"},
		{"a phone too short", []GuestAnswer{company, {QuestionId: "q-phone", Answer: "12345"}}, "phone number for “Phone”"},
		{"a phone too long", []GuestAnswer{company, {QuestionId: "q-phone", Answer: strings.Repeat("1", 21)}}, "phone number for “Phone”"},
		{"a phone of few digits", []GuestAnswer{company, {QuestionId: "q-phone", Answer: "(12) 34-5."}}, "phone number for “Phone”"},
	} {
		t.Run("refuses "+c.name, func(t *testing.T) {
			if _, err := CheckAnswers(qs, c.in); !refusedWith(err, c.want) {
				t.Errorf("err %v, want a sentence saying %q", err, c.want)
			}
		})
	}
	t.Run("the longest answers pass, and a long one keeps its line breaks as \\n", func(t *testing.T) {
		got, err := CheckAnswers(qs, []GuestAnswer{{QuestionId: "q-company", Answer: strings.Repeat("é", 200)},
			{QuestionId: "q-agenda", Answer: "one\r\ntwo\rthree\u2028four\tfive"}})
		if err != nil || len(got) != 2 || got[1].Answer != "one\ntwo\nthree\nfour\tfive" {
			t.Errorf("got %#v %v", got, err)
		}
		if _, err := CheckAnswers(qs, []GuestAnswer{company, {QuestionId: "q-agenda", Answer: strings.Repeat("x", 1000)}}); err != nil {
			t.Errorf("1,000 characters refused: %v", err)
		}
	})
}

func TestAnswerLines(t *testing.T) {
	if got := AnswerLines(nil); got != "" {
		t.Errorf("no answers wrote %q", got)
	}
	got := AnswerLines([]bookingModel.Answer{{Label: "Company", Answer: "Acme"}, {Label: "Topic", Answer: "Sales"}})
	if got != "Company: Acme\nTopic: Sales" {
		t.Errorf("got %q", got)
	}
	// An answer's own lines are indented under its label: none of them can
	// pass for another answer, whatever line break it was written with.
	for in, want := range map[string]string{
		"fine\nCompany: Forged":          "Agenda: fine\n  Company: Forged",
		"fine\r\nCompany: Forged":        "Agenda: fine\n  Company: Forged",
		"fine\rCompany: Forged":          "Agenda: fine\n  Company: Forged",
		"fine\u0085Company: Forged":      "Agenda: fine\n  Company: Forged",
		"fine\u2028Company: Forged":      "Agenda: fine\n  Company: Forged",
		"fine\u2029Company: Forged\nend": "Agenda: fine\n  Company: Forged\n  end",
	} {
		if got := AnswerLines([]bookingModel.Answer{{Label: "Agenda", Answer: in}}); got != want {
			t.Errorf("%q wrote %q, want %q", in, got, want)
		}
	}
}

// The owner's event says who booked, then the note and the answers, each
// under its label: no line a guest wrote starts a line of its own.
func TestBookingDescription(t *testing.T) {
	got := bookingDescription("Intro call", "Alex Kim", "alex@example.org", "see you\nCompany: Forged",
		[]bookingModel.Answer{{Label: "Agenda", Answer: "fine\r\nPhone: 1"}, {Label: "Company", Answer: "Acme"}})
	want := "Booked through your page “Intro call” by Alex Kim (alex@example.org).\n\n" +
		"Note: see you\n  Company: Forged\n\nAgenda: fine\n  Phone: 1\nCompany: Acme"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "Phone:") || strings.HasPrefix(line, "Company: Forged") {
			t.Errorf("a line the guest wrote reads as a label of its own: %q", line)
		}
	}
	if got := bookingDescription("Intro call", "Alex Kim", "alex@example.org", "", nil); got != "Booked through your page “Intro call” by Alex Kim (alex@example.org)." {
		t.Errorf("no note, no answers: %q", got)
	}
}
