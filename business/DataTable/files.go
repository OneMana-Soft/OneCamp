package business

// A table's files (tables B1, the files kind): attachments whose place is
// the table (business/FileAccess), which a files cell names by id.

import (
	"context"

	postgresStruct "github.com/akashc777/OneCamp/models/postgres"
	attachmentModels "github.com/akashc777/OneCamp/models/postgres/Attachment"
	"github.com/google/uuid"
)

// filesOfPlace is the attachments lookup, a seam for tests.
var filesOfPlace = attachmentModels.FilesOfPlace

// filesOf is how a write to table's rows asks which files are the table's
// (fieldTypes.Input.Files).
func filesOf(ctx context.Context, table uuid.UUID) func(ids []string) (map[string]bool, error) {
	return func(ids []string) (map[string]bool, error) {
		return filesOfPlace(ctx, postgresStruct.ATTACHMENT_SRC_TABLE, table.String(), ids)
	}
}
