package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// Question is one a booking page asks whoever books it (gcal-7 H3). Its id is
// the page's own, kept across edits and reorders.
type Question struct {
	Id       string `json:"id"`
	Label    string `json:"label"`
	Kind     string `json:"kind"`
	Required bool   `json:"required"`
	// Options are a choice's answers to pick from: none for another kind.
	Options []string `json:"options,omitempty"`
}

// The kinds of answer a question takes.
const (
	QuestionShort  = "short"
	QuestionLong   = "long"
	QuestionChoice = "choice"
	QuestionPhone  = "phone"
)

// Answer is what a guest answered, kept with the question's label as it was
// then: a later edit of the question never changes what they answered.
type Answer struct {
	QuestionId string `json:"question_id"`
	Label      string `json:"label"`
	Answer     string `json:"answer"`
}

// Questions are a page's questions, its questions column.
type Questions []Question

// Answers are a booking's answers, its answers column.
type Answers []Answer

// Scan reads the questions column.
func (q *Questions) Scan(src any) error { return scanList(src, q) }

// Value writes the questions column.
func (q Questions) Value() (driver.Value, error) { return listValue(q, len(q)) }

// Scan reads the answers column.
func (a *Answers) Scan(src any) error { return scanList(src, a) }

// Value writes the answers column.
func (a Answers) Value() (driver.Value, error) { return listValue(a, len(a)) }

// scanList reads a jsonb list column into dst.
func scanList(src, dst any) error {
	switch v := src.(type) {
	case []byte:
		return json.Unmarshal(v, dst)
	case string:
		return json.Unmarshal([]byte(v), dst)
	}
	return fmt.Errorf("booking: a list column read as %T", src)
}

// listValue writes list, of n items, as a jsonb list: [] for none, never null.
func listValue(list any, n int) (driver.Value, error) {
	if n == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(list)
	return string(b), err
}
