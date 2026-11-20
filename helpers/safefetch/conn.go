package safefetch

import (
	"errors"
	"net"
	"sync/atomic"
)

// ErrReadBudget is a connection that read all it may: the far end sent more
// than anything asked of it can take.
var ErrReadBudget = errors.New("safefetch: the connection read past its budget")

// LimitReads is c reading at most n bytes in all; past them a read fails with
// ErrReadBudget and c is closed. It caps what a server this server dialled
// may push at it, whatever the protocol says is coming (an IMAP server sends
// a literal of any length, whatever size it declared).
func LimitReads(c net.Conn, n int64) net.Conn {
	b := &budgetConn{Conn: c}
	b.left.Store(n)
	return b
}

type budgetConn struct {
	net.Conn
	left atomic.Int64
}

func (c *budgetConn) Read(p []byte) (int, error) {
	left := c.left.Load()
	if left <= 0 {
		_ = c.Conn.Close()
		return 0, ErrReadBudget
	}
	if int64(len(p)) > left {
		p = p[:left]
	}
	n, err := c.Conn.Read(p)
	c.left.Add(-int64(n))
	return n, err
}
