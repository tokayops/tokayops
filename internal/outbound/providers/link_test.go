package providers

import "testing"

// TestLinkableRefusesWhatABrowserWouldRewrite. The same addresses are refused
// in the web UI, from the raw string, before either parser normalises them.
func TestLinkableRefusesWhatABrowserWouldRewrite(t *testing.T) {
	accepted := `https://grafana.example/d/a?var-host="db"&from=now`
	if !Linkable(accepted) {
		t.Fatalf("a query a dashboard actually uses was refused: %s", accepted)
	}
	if !Linkable("http://grafana.example/d/abc") {
		t.Fatal("an http dashboard was refused")
	}

	refused := []string{
		"https://a b.example",
		"https://a\nb.example",
		"https://a<b.example",
		"https://a>b.example",
		"https://a|b.example",
		"/ops",
		"//evil.example",
		"javascript:alert(1)",
		"https:example.com",
		"https://",
		"https:///example.com",
		`https:\\example.com`,
		`https://example.com\path`,
		`https://evil.example\@good.example`,
		"ftp://files.example/runbook",
		"",
	}
	for _, raw := range refused {
		if Linkable(raw) {
			t.Errorf("%q was accepted", raw)
		}
	}
}
