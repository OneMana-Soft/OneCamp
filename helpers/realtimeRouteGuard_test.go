package helpers

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Real-time messages are routed on the port the app connects to.
//
// WHAT WENT WRONG. The installer writes MQTT_WS_URL=wss://<host>/mqtt, with no
// port, and the backend hands that URL to every browser, so they dial 443. The
// shipped compose file routed that host on its 8084 entrypoint alone, so on
// every install made from it nothing arrived live: messages, typing, presence
// all waited for a reload. (The demo runs final-compose.yml, which routes it on
// 443, so it never showed there; a proxy in front, as on Cloud, cannot carry
// 8084 at all.)
func TestRealtimeIsRoutedWhereTheAppConnects(t *testing.T) {
	mk, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatal(err)
	}
	urls := regexp.MustCompile(`set_env,MQTT_WS_URL,wss://([^/\s]+)/mqtt`).FindAllStringSubmatch(string(mk), -1)
	if len(urls) == 0 {
		t.Fatal("Makefile-distribute no longer writes MQTT_WS_URL; update this test to where the app's URL comes from")
	}
	for _, u := range urls {
		if regexp.MustCompile(`:\d+$`).MatchString(u[1]) {
			t.Fatalf("the installer now writes a port into MQTT_WS_URL (%s); check the routers below agree with it", u[1])
		}
	}
	router := regexp.MustCompile(`traefik\.http\.routers\.emqx-ws-https\.entrypoints=([a-z0-9,]+)`)
	for _, f := range []string{"../distribute-compose.yml", "../final-compose.yml"} {
		c, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		m := router.FindSubmatch(c)
		if m == nil {
			t.Errorf("%s has no real-time router", f)
			continue
		}
		on443 := false
		for _, ep := range strings.Split(string(m[1]), ",") {
			if ep == "https" {
				on443 = true
			}
		}
		if !on443 {
			t.Errorf("%s routes real-time on %q only, but the app connects on 443 (the https entrypoint)", f, m[1])
		}
	}
}
