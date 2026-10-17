package mqttInit

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	MQTT "github.com/eclipse/paho.mqtt.golang"
)

// flakyBroker stands in for a broker that refuses the first failures CONNECTs.
type flakyBroker struct {
	MQTT.Client
	failures  int32
	attempts  atomic.Int32
	connected atomic.Bool
	// acceptWithoutConnecting answers a CONNECT as accepted but leaves the
	// client unconnected, as the library does for a CONNECT made while an
	// earlier one is still in flight.
	acceptWithoutConnecting bool
}

func (b *flakyBroker) Connect() MQTT.Token {
	n := b.attempts.Add(1)
	if n <= b.failures {
		return doneToken{err: errors.New("connection refused")}
	}
	if !b.acceptWithoutConnecting {
		b.connected.Store(true)
	}
	return doneToken{}
}

func (b *flakyBroker) IsConnected() bool { return b.connected.Load() }

type doneToken struct{ err error }

func (doneToken) Wait() bool                     { return true }
func (doneToken) WaitTimeout(time.Duration) bool { return true }
func (doneToken) Done() <-chan struct{}          { c := make(chan struct{}); close(c); return c }
func (t doneToken) Error() error                 { return t.err }

func quietLogs(t *testing.T) {
	t.Helper()
	if helpers.MessageLogs == nil {
		discard := log.New(io.Discard, "", 0)
		helpers.MessageLogs = &helpers.Message{InfoLog: discard, ErrorLog: discard}
	}
}

func fastBackoff(t *testing.T) {
	t.Helper()
	base, max := connectBackoffBase, connectBackoffMax
	connectBackoffBase, connectBackoffMax = time.Millisecond, 4*time.Millisecond
	t.Cleanup(func() { connectBackoffBase, connectBackoffMax = base, max })
}

// A broker that refuses at first is tried again, with backoff, until it
// answers: before, one refusal was logged and never retried.
func TestTheFirstConnectionIsTriedAgainUntilTheBrokerAnswers(t *testing.T) {
	quietLogs(t)
	fastBackoff(t)
	b := &flakyBroker{failures: 3}
	if err := ConnectWithBackoff(context.Background(), b, CurrentConnectTimings()); err != nil {
		t.Fatalf("ConnectWithBackoff: %v", err)
	}
	if got := b.attempts.Load(); got != 4 {
		t.Errorf("CONNECT was tried %d times; want 4 (three refusals, then the answer)", got)
	}
	if !b.IsConnected() {
		t.Error("ConnectWithBackoff returned, but the client isn't connected")
	}
}

// The caller's context bounds the whole wait, and the error says how many
// tries were made and why the last one failed.
func TestConnectingGivesUpWhenItsContextEnds(t *testing.T) {
	quietLogs(t)
	fastBackoff(t)
	b := &flakyBroker{failures: 1 << 30}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	err := ConnectWithBackoff(ctx, b, CurrentConnectTimings())
	if err == nil {
		t.Fatal("ConnectWithBackoff succeeded against a broker that never answers")
	}
	if !strings.Contains(err.Error(), "no connection to the broker after") || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("the error doesn't say what happened: %v", err)
	}
	if b.attempts.Load() < 2 {
		t.Errorf("only %d tries in 40 ms with a 1-4 ms backoff", b.attempts.Load())
	}
}

// A CONNECT answered as accepted with nothing connected is not a connection.
func TestAnAcceptedConnectThatConnectedNothingIsTriedAgain(t *testing.T) {
	quietLogs(t)
	fastBackoff(t)
	b := &flakyBroker{acceptWithoutConnecting: true}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := ConnectWithBackoff(ctx, b, CurrentConnectTimings()); err == nil {
		t.Fatal("ConnectWithBackoff took an accepted CONNECT with no connection for a connection")
	}
}

// An effort to connect runs on the timings it was handed, read once before it
// started, as ConnectMqtt reads them. One left trying in the background reads
// none of the variables afterwards, so a test can put them back without
// waiting for it, and the server needs nothing that exists only for a test to
// wait on. Reading them on every try instead fails here: a race under -race,
// and without it a stall behind the hour-long pause.
func TestAConnectionEffortKeepsTheTimingsItStartedWith(t *testing.T) {
	quietLogs(t)
	fastBackoff(t)
	b := &flakyBroker{failures: 1 << 30}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	tm := CurrentConnectTimings()
	go func() {
		defer close(done)
		_ = ConnectWithBackoff(ctx, b, tm)
	}()
	// Runs before fastBackoff puts the timings back (cleanups run last first).
	t.Cleanup(func() { cancel(); <-done })
	waitForTries := func(n int32) {
		t.Helper()
		// The bound only stops a broken build from hanging the run.
		deadline := time.Now().Add(10 * time.Second)
		for b.attempts.Load() < n {
			if time.Now().After(deadline) {
				t.Fatalf("%d tries after 10 s; want at least %d", b.attempts.Load(), n)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitForTries(2)
	connectBackoffBase, connectBackoffMax = time.Hour, time.Hour
	waitForTries(b.attempts.Load() + 3)
}

// The server starts without the broker, with a client that is never nil, and
// keeps trying in the background; the realtime check says it isn't connected.
func TestTheServerStartsWithoutTheBrokerAndKeepsTrying(t *testing.T) {
	quietLogs(t)
	fastBackoff(t)
	wait, bg, attempt := bootConnectWait, backgroundConnect, connectAttemptTimeout
	ctx, cancel := context.WithCancel(context.Background())
	bootConnectWait, backgroundConnect, connectAttemptTimeout = 30*time.Millisecond, ctx, 200*time.Millisecond
	t.Cleanup(func() { bootConnectWait, backgroundConnect, connectAttemptTimeout = wait, bg, attempt })
	before := MqttClient
	t.Cleanup(func() { MqttClient = before })
	// The background tries took their timings when they started, so putting
	// the variables back can't race with them; cancelling ends them.
	t.Cleanup(cancel)

	// Nothing listens on port 1.
	if err := ConnectMqtt(&MqttConfig{Broker: "tcp://127.0.0.1:1", ClientID: "test", Username: "backend", CleanSession: true}); err != nil {
		t.Fatalf("ConnectMqtt failed the start-up: %v", err)
	}
	if MqttClient == nil {
		t.Fatal("ConnectMqtt left MqttClient nil")
	}
	if MqttClient.IsConnected() {
		t.Fatal("connected to a broker that isn't there")
	}
	check := realtimeCheck()
	if err := check.Probe(context.Background()); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Errorf("the realtime check says %v; want it to say the broker isn't connected", err)
	}
}
