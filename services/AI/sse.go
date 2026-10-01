package ai

// One reader for the server-sent event streams this server consumes.
//
// Two of them had grown their own: the MCP client, whose JSON-RPC replies
// arrive as SSE when a server chooses that transport, and the AG-UI client,
// which is SSE by definition. Both did the same dance (collect data lines,
// flush on a blank line, join with newlines) and only one of them got the
// spec's details right. A stream reader is exactly the kind of code where a
// second copy is where the bug lives, so there is one.
//
// The provider streaming paths (OpenAI, Anthropic) are deliberately NOT moved
// onto this: they read a format their provider fixes to one line per frame and
// carry their own sentinel handling, and rewriting a hot streaming loop to
// gain nothing is not a simplification.

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
)

// ForEachSSEFrame walks a text/event-stream, calling fn once per frame with
// that frame's data payload.
//
// Multi-line data is joined with newlines and one optional leading space is
// stripped from each line, both as the spec requires; comment lines and the
// other fields (event, id, retry) are skipped, because every caller here keys
// on what is inside the payload rather than on the frame's name.
//
// fn returning stop ends the walk with no error, for a caller that has what it
// came for and should not drain the rest of the stream. A frame still open at
// EOF is delivered, so a server that ends without a trailing blank line does
// not lose its last event.
//
// maxFrameBytes bounds a single line, so a peer cannot make this allocate
// without limit.
func ForEachSSEFrame(r io.Reader, maxFrameBytes int, fn func(data []byte) (stop bool, err error)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxFrameBytes)

	var data [][]byte
	flush := func() (bool, error) {
		if len(data) == 0 {
			return false, nil
		}
		joined := bytes.Join(data, []byte("\n"))
		data = nil
		return fn(joined)
	}

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			stop, err := flush()
			if stop || err != nil {
				return err
			}
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		if string(field) != "data" {
			continue
		}
		value = bytes.TrimPrefix(value, []byte(" "))
		data = append(data, append([]byte(nil), value...))
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("reading the event stream: %w", err)
	}
	_, err := flush()
	return err
}
