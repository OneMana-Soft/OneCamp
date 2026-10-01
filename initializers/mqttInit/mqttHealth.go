package mqttInit

// Is realtime actually live?
//
// WHAT BREAKS WITHOUT IT. Messages, typing indicators and presence all travel
// over MQTT. When the broker is unreachable the product does not error: it goes
// quiet. Messages still save, and simply never arrive until the page is
// reloaded, which reads to a user as "this chat app does not work" and to an
// operator as nothing at all, because the boot log said "Successfully connected"
// once and was never asked again.
//
// A client that connected at boot and dropped afterwards is the common case:
// EMQX restarts, a container is recreated, credentials rotate. The library
// reconnects on its own when it can, so this reports the state now rather than
// the state at boot.

import (
	"context"
	"fmt"

	"github.com/akashc777/OneCamp/helpers"
)

func init() {
	helpers.RegisterSystemCheck(helpers.SystemCheck{
		Name: "realtime",
		Kind: helpers.CheckKindDependency,
		Describe: "The MQTT broker is connected right now, so messages, typing and presence reach people " +
			"live. It does not prove any individual topic is permitted by the broker's ACL.",
		Probe: func(_ context.Context) error {
			if MqttClient == nil {
				return fmt.Errorf("no MQTT client: realtime was never initialised, so messages will only appear on reload")
			}
			if !MqttClient.IsConnected() {
				return fmt.Errorf("not connected to the MQTT broker; messages will save but will not appear " +
					"until the page is reloaded, and presence and typing will be dead")
			}
			return nil
		},
	})
}
