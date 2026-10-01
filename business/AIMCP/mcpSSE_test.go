package business

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// failAfter yields s and then fails. It proves the reader stopped where it
// said it did: anything that kept draining the stream would hit the error.
type failAfter struct {
	r    io.Reader
	done bool
}

func (f *failAfter) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err == io.EOF {
		f.done = true
		return n, errors.New("the stream was read past the response")
	}
	return n, err
}

// An MCP server may answer over SSE, with the reply among unrelated
// notifications and its JSON split across data lines.
func TestParseSSEResponseFindsOurReplyAmongTheRest(t *testing.T) {
	stream := ": keepalive\n" +
		"event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":7,\n" +
		"data: \"result\":{\"ok\":true}}\n\n"

	resp, err := parseSSEResponse(strings.NewReader(stream), 7)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Result == nil || !strings.Contains(string(resp.Result), `"ok":true`) {
		t.Errorf("result = %s", resp.Result)
	}
}

// It stops at our reply rather than draining whatever the server keeps
// sending, which for a long-lived stream is the difference between a call that
// returns and one that hangs.
func TestParseSSEResponseStopsAtOurReply(t *testing.T) {
	stream := "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n"
	if _, err := parseSSEResponse(&failAfter{r: strings.NewReader(stream)}, 1); err != nil {
		t.Errorf("the reader kept going past the reply: %v", err)
	}
}

// A reply to somebody else's id is not ours, and a stream carrying none says
// so rather than returning an empty response.
func TestParseSSEResponseIgnoresAnotherRequestsReply(t *testing.T) {
	stream := "data: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n\n"
	if _, err := parseSSEResponse(strings.NewReader(stream), 1); err == nil {
		t.Error("a reply to request 2 was accepted as the answer to request 1")
	}
	if _, err := parseSSEResponse(strings.NewReader("data: not json\n\n"), 1); err == nil {
		t.Error("a stream with no reply must say so")
	}
}

// An error reply is a reply: it has to come back so the caller can report what
// the server said, rather than timing out looking for a result.
func TestParseSSEResponseReturnsAnErrorReply(t *testing.T) {
	stream := "data: {\"jsonrpc\":\"2.0\",\"id\":3,\"error\":{\"code\":-32601,\"message\":\"no such tool\"}}\n\n"
	resp, err := parseSSEResponse(strings.NewReader(stream), 3)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "no such tool") {
		t.Errorf("error = %+v", resp.Error)
	}
}
