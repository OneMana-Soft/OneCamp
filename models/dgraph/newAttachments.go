package models

import (
	"time"

	"github.com/google/uuid"
)

// NewAttachments is the files a request adds (to a message, a comment, a task,
// a project or a forward) as new attachment nodes, added by byUID at at.
//
// A request's file is what a member uploaded: its id, name, stored object,
// type and size. It is never a node the graph already holds. Those writes
// used to send the request's file as it came, so a uid in it named an
// existing node (another member's file, a private doc, anything), linked that
// node into the new message and wrote the request's fields over it, and a
// nested attachment_project, attachment_task or attachment_created_by did the
// same to its own node. Here only the upload's own fields are kept: no uid,
// no nested node, the adder and the time from the server, and an id that is
// a uuid (a new one when the request's isn't). A nil file is dropped.
func NewAttachments(in []*DgraphAttachment, byUID string, at time.Time) []*DgraphAttachment {
	if in == nil {
		return nil
	}
	out := make([]*DgraphAttachment, 0, len(in))
	for _, f := range in {
		if f == nil {
			continue
		}
		id := f.Uuid
		if parsed, err := uuid.Parse(id); err != nil {
			id = uuid.NewString()
		} else {
			id = parsed.String()
		}
		created := at
		out = append(out, &DgraphAttachment{
			Uuid:      id,
			FileName:  f.FileName,
			ObjectKey: f.ObjectKey,
			Type:      f.Type,
			RawType:   f.RawType,
			Width:     f.Width,
			Size:      f.Size,
			Hight:     f.Hight,
			Duration:  f.Duration,
			DType:     []string{"Attachment"},
			CreatedAt: &created,
			CreatedBy: &DgraphUser{Uid: byUID},
		})
	}
	return out
}
