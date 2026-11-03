package business

import "testing"

func TestAnAuditRowKeepsNoKeyFromAnAddress(t *testing.T) {
	for raw, want := range map[string]string{
		"https://user:tok-secret@runner.example.com/v1?key=sk-secret#tok-secret": "https://runner.example.com/v1",
		"https://tok-secret@runner.example.com":                                  "https://runner.example.com",
		"  https://models.example.test/v1  ":                                     "https://models.example.test/v1",
		"http://10.0.0.5:8080/run?":                                              "http://10.0.0.5:8080/run",
		"":                                                                       "",
		"http://a b.example.com/?key=sk-secret":                                  "(unreadable address)",
		"   ":                                                                    "",
		"admin:sk-secret@runner.local/x":                                         "(unreadable address)",
		"mailto:sk-secret@runner.local":                                          "(unreadable address)",
		"runner.local:8080/run?key=sk-secret":                                    "(unreadable address)",
		"/run?key=sk-secret":                                                     "(unreadable address)",
		"https://tok:sk-secret@:8080/run":                                        "(unreadable address)",
		"file:///etc/sk-secret":                                                  "(unreadable address)",
	} {
		if got := URLForRecord(raw); got != want {
			t.Errorf("URLForRecord(%q) = %q, want %q", raw, got, want)
		}
	}
}
