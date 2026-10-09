package helpers

import "context"

// publicSubmissionKey marks work that files what a visitor sent in.
type publicSubmissionKey struct{}

// WithPublicSubmission marks ctx as filing what a visitor sent through a public
// page, an intake form say. The visitor is nobody the workspace knows, and what
// they wrote is filed as the page's owner, who wrote none of it. Everything
// downstream inherits the mark.
func WithPublicSubmission(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, publicSubmissionKey{}, true)
}

// IsPublicSubmission reports whether this work files a visitor's submission.
func IsPublicSubmission(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(publicSubmissionKey{}).(bool)
	return v
}
