package scene

// Frames is a board's frames, read from its elements root map (Maps'
// "elements"), by id: true for a frame on the canvas, false for one deleted
// there and still in the document, where undo can bring it back. An id that
// isn't here is no frame in the document: gone (the collaboration service
// prunes a deleted element after a while), or never one. Pure.
func Frames(elements map[string]any) map[string]bool {
	out := map[string]bool{}
	for id, v := range elements {
		el, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := el["type"].(string); kind == "frame" || kind == "magicframe" {
			out[id] = el["isDeleted"] != true
		}
	}
	return out
}
