//go:build integration
// +build integration

package integration

// The server's MQTT client in the integration binary: never nil.
//
// Code under test publishes from goroutines it doesn't wait for (58 of them
// start with a bare `go mqttBusiness.Publish…`), and one that ran after a
// test had set the global back to nil dereferenced it and took the whole test
// binary down: a crash, not a failure, and every test after it unrun. In
// production the client is never nil either: ConnectMqtt sets it before it
// connects, and a broker that is down leaves it unconnected. So that is the
// shape a test without a broker gets, here from the start and again after
// every test that put its own client in.

import (
	"testing"

	MQTT "github.com/eclipse/paho.mqtt.golang"

	mqttInit "github.com/akashc777/OneCamp/initializers/mqttInit"
)

func init() {
	if mqttInit.MqttClient == nil {
		mqttInit.MqttClient = unconnectedMqttClient()
	}
}

// unconnectedMqttClient is a client that never connects: publishing on it
// answers ErrNotConnected at once instead of panicking or waiting. Port 1, so
// nothing can connect to it by accident.
func unconnectedMqttClient() MQTT.Client {
	return MQTT.NewClient(MQTT.NewClientOptions().AddBroker("tcp://127.0.0.1:1"))
}

// UseMqttClient makes c the server's MQTT client for the rest of the test.
// When the test ends, once the work it handed off has settled, the server has
// an unconnected client again.
func UseMqttClient(t *testing.T, c MQTT.Client) {
	t.Helper()
	ensureLoggers()
	mqttInit.MqttClient = c
	t.Cleanup(func() { mqttInit.MqttClient = unconnectedMqttClient() })
	settleBeforeTeardown(t)
}

// StubMqttClient gives the test a client without a broker: publishing works,
// and nothing is delivered. For a test about what the broker holds or
// delivers, SetupBroker.
func StubMqttClient(t *testing.T) {
	t.Helper()
	UseMqttClient(t, unconnectedMqttClient())
}
