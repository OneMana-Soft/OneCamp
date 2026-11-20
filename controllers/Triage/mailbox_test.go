package controllers

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http/httptest"
	"syscall"
	"testing"

	triageMail "github.com/akashc777/OneCamp/business/TriageMail"
	"github.com/akashc777/OneCamp/helpers"
)

// A mailbox Test answers an address it may not reach (one on this server's
// own network) exactly as it answers one that doesn't resolve or doesn't
// answer: one status, one message, so a project admin can't map the
// network's internal names by testing them (CR-409 N2).
func TestAMailboxTestCantTellAPrivateAddressFromAnUnreachableOne(t *testing.T) {
	prev, discard := helpers.MessageLogs, log.New(io.Discard, "", 0)
	helpers.MessageLogs = &helpers.Message{InfoLog: discard, ErrorLog: discard}
	t.Cleanup(func() { helpers.MessageLogs = prev })
	answer := func(err error) (int, string) {
		rec := httptest.NewRecorder()
		writeMailErr(rec, err)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		msg, _ := out["msg"].(string)
		return rec.Code, msg
	}
	code, msg := answer(triageMail.ErrRefused)
	for _, err := range []error{
		&net.DNSError{Err: "no such host", Name: "mail.internal.example", IsNotFound: true},
		&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
		&net.DNSError{Err: "i/o timeout", Name: "mail.example.com", IsTimeout: true},
	} {
		if c, m := answer(err); c != code || m != msg {
			t.Errorf("%T %v answers %d %q; a refused private address answers %d %q: want the same", err, err, c, m, code, msg)
		}
	}
}
