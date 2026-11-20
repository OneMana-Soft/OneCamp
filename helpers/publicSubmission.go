package helpers

import "context"

// publicSubmissionKey marks work that files what a stranger sent in; its value
// is the door it came through.
type publicSubmissionKey struct{}

// FormDoor is the door WithPublicSubmission names: a public page, an intake
// form say.
const FormDoor = "form"

// WithPublicSubmission marks ctx as filing what a visitor sent through a public
// page, an intake form say. The visitor is nobody the workspace knows, and what
// they wrote is filed as the page's owner, who wrote none of it. Everything
// downstream inherits the mark.
func WithPublicSubmission(ctx context.Context) context.Context {
	return WithSubmissionDoor(ctx, FormDoor)
}

// WithSubmissionDoor is WithPublicSubmission for what came through door ("mail"
// for the triage mail door): the same mark, naming where the words came from.
// An empty door is FormDoor.
func WithSubmissionDoor(ctx context.Context, door string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if door == "" {
		door = FormDoor
	}
	return context.WithValue(ctx, publicSubmissionKey{}, door)
}

// IsPublicSubmission reports whether this work files a stranger's submission.
func IsPublicSubmission(ctx context.Context) bool {
	return SubmissionDoor(ctx) != ""
}

// SubmissionDoor is the door the submission this work files came through, or
// "" when it files none.
func SubmissionDoor(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(publicSubmissionKey{}).(string)
	return v
}
