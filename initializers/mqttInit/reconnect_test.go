package mqttInit

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// After it loses the broker, the server's client comes back by itself and
// subscribes again, and the realtime check (what /health/ready, verify and
// wait-ready ask) says it isn't connected until it has.
//
// WHY. The library reconnects on its own, and while it does IsConnected
// answers true: it means "connected, or will be". The check asked
// IsConnected, so readiness said realtime was ok while nothing reached
// anyone. A connection is lost loudly when the broker restarts (the socket
// closes) and silently when the machine sleeps or the broker stops answering
// (nothing closes, and only the keepalive finds out). Both are tested here,
// against a broker that speaks just enough MQTT 3.1.1.
func TestTheServerComesBackAfterLosingTheBroker(t *testing.T) {
	quietLogs(t)
	fastBackoff(t)
	quickKeepalive(t)

	b := startFakeBroker(t)
	before := MqttClient
	t.Cleanup(func() {
		if MqttClient != nil {
			MqttClient.Disconnect(0)
		}
		MqttClient = before
	})
	if err := ConnectMqtt(&MqttConfig{Broker: "tcp://" + b.addr, ClientID: "test-reconnect", Username: "backend", CleanSession: true}); err != nil {
		t.Fatalf("ConnectMqtt: %v", err)
	}
	b.wantSubscribed(t, "on the first connection")
	realtimeSays(t, true, "once connected")

	// A broker restarting: its sockets close, nothing listens for a while,
	// then it does again at the same address.
	b.down()
	realtimeSays(t, false, "while the broker is down")
	b.up(t)
	b.wantSubscribed(t, "after the broker came back")
	realtimeSays(t, true, "after the broker came back")

	// A machine that slept, or a broker that hangs: the connection stays open
	// and nothing answers on it.
	b.mute(true)
	realtimeSays(t, false, "while the broker doesn't answer")
	b.mute(false)
	b.wantSubscribed(t, "after the broker answered again")
	realtimeSays(t, true, "after the broker answered again")
}

// quickKeepalive has the client ping after a second of quiet and give up on
// an unanswered ping, or an unanswered CONNECT, after a moment.
func quickKeepalive(t *testing.T) {
	t.Helper()
	attempt, alive, ping := connectAttemptTimeout, keepAlive, pingTimeout
	connectAttemptTimeout, keepAlive, pingTimeout = 300*time.Millisecond, time.Second, 300*time.Millisecond
	t.Cleanup(func() { connectAttemptTimeout, keepAlive, pingTimeout = attempt, alive, ping })
}

// realtimeSays waits for the realtime check to pass (or fail), for up to ten
// seconds.
func realtimeSays(t *testing.T, connected bool, when string) {
	t.Helper()
	check := realtimeCheck()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := check.Probe(context.Background())
		if (err == nil) == connected {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s the realtime check says %v", when, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// fakeBroker accepts CONNECT, grants SUBSCRIBE and answers PINGREQ: enough
// MQTT 3.1.1 for the server's client.
type fakeBroker struct {
	addr  string
	subs  chan string // each topic filter subscribed to, in order
	muted atomic.Bool

	mu    sync.Mutex
	ln    net.Listener
	conns map[net.Conn]struct{}
}

func startFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeBroker{addr: ln.Addr().String(), subs: make(chan string, 256), conns: map[net.Conn]struct{}{}}
	b.serve(ln)
	t.Cleanup(b.down)
	return b
}

func (b *fakeBroker) serve(ln net.Listener) {
	b.mu.Lock()
	b.ln = ln
	b.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			b.mu.Lock()
			b.conns[c] = struct{}{}
			b.mu.Unlock()
			go b.handle(c)
		}
	}()
}

// down stops listening and closes every connection.
func (b *fakeBroker) down() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ln != nil {
		_ = b.ln.Close()
		b.ln = nil
	}
	for c := range b.conns {
		_ = c.Close()
	}
	clear(b.conns)
}

// up listens again at the same address.
func (b *fakeBroker) up(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", b.addr)
	if err != nil {
		t.Fatalf("listen again at %s: %v", b.addr, err)
	}
	b.serve(ln)
}

// mute stops answering (or starts again), keeping every connection open.
func (b *fakeBroker) mute(on bool) {
	b.drain()
	b.muted.Store(on)
}

func (b *fakeBroker) drain() {
	for {
		select {
		case <-b.subs:
		default:
			return
		}
	}
}

// wantSubscribed waits for a connection to subscribe to both of the broker's
// client announcements.
func (b *fakeBroker) wantSubscribed(t *testing.T, when string) {
	t.Helper()
	want := map[string]bool{clientConnectedTopic: false, clientDisconnectedTopic: false}
	deadline := time.After(10 * time.Second)
	for missing := len(want); missing > 0; {
		select {
		case topic := <-b.subs:
			if seen, ok := want[topic]; ok && !seen {
				want[topic] = true
				missing--
			}
		case <-deadline:
			t.Fatalf("%s the client didn't subscribe: %v", when, want)
		}
	}
}

func (b *fakeBroker) handle(c net.Conn) {
	defer func() {
		_ = c.Close()
		b.mu.Lock()
		delete(b.conns, c)
		b.mu.Unlock()
	}()
	r := bufio.NewReader(c)
	for {
		head, err := r.ReadByte()
		if err != nil {
			return
		}
		body, err := readPacketBody(r)
		if err != nil {
			return
		}
		if b.muted.Load() {
			continue
		}
		var answer []byte
		switch head >> 4 {
		case 1: // CONNECT
			answer = []byte{0x20, 0x02, 0x00, 0x00}
		case 8: // SUBSCRIBE: a packet id, then filters, each length-prefixed and followed by a QoS
			if len(body) < 2 {
				return
			}
			answer = []byte{0x90, 0x02, body[0], body[1]}
			for p := body[2:]; len(p) >= 3; {
				n := int(p[0])<<8 | int(p[1])
				if len(p) < 2+n+1 {
					return
				}
				b.subs <- string(p[2 : 2+n])
				answer = append(answer, p[2+n]&0x03)
				answer[1]++
				p = p[2+n+1:]
			}
		case 12: // PINGREQ
			answer = []byte{0xD0, 0x00}
		case 14: // DISCONNECT
			return
		}
		if answer != nil {
			if _, err := c.Write(answer); err != nil {
				return
			}
		}
	}
}

// readPacketBody reads a packet's remaining length, then that many bytes.
func readPacketBody(r *bufio.Reader) ([]byte, error) {
	n, scale := 0, 1
	for i := 0; i < 4; i++ {
		d, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		n += int(d&0x7f) * scale
		if d&0x80 == 0 {
			body := make([]byte, n)
			_, err := io.ReadFull(r, body)
			return body, err
		}
		scale *= 128
	}
	return nil, errors.New("malformed remaining length")
}
