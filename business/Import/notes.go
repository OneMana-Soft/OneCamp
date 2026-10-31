package business

// What a chunk has to say in its import's report, held while it runs and
// written together: a thousand notes to a statement
// (importModels.LogImportErrors). A note was a statement of its own, and a
// base's notes aren't capped (one for each cell its importer leaves out), so
// a messy base ran thousands.

import (
	"context"

	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// chunkNotes is a chunk's notes, held for its import's report.
type chunkNotes struct {
	job   uuid.UUID
	chunk *uuid.UUID
	held  []importModels.ImportError
}

func newChunkNotes(job uuid.UUID, chunk *uuid.UUID) *chunkNotes {
	return &chunkNotes{job: job, chunk: chunk}
}

// add holds a note about something the chunk read (entity, by its source id).
func (n *chunkNotes) add(entity, source, severity, code, msg string) {
	n.held = append(n.held, importModels.ImportError{ChunkID: n.chunk, EntityType: entity, SourceID: source,
		Severity: severity, Code: code, Message: msg})
}

// flush writes the notes held, and lets them go: even once ctx has ended, as
// a stopping worker's notes are still its report.
func (n *chunkNotes) flush(ctx context.Context) {
	if len(n.held) == 0 {
		return
	}
	importModels.LogImportErrors(context.WithoutCancel(ctx), n.job, n.held)
	n.held = nil
}
