package models

import (
	"context"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
)

// ForgetEntry removes one entry from a search index. For content its database
// says no longer exists (see domain/Liveness), never for content that is only
// soft-deleted, whose entry must survive a restore from trash.
func ForgetEntry(ctx context.Context, index, id string) error {
	return opensearchInit.DeleteDocument(ctx, index, id)
}
