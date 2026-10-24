package i18n

import "testing"

func TestMatch(t *testing.T) {
	for header, want := range map[string]Lang{
		"":                        English,
		"nl-BE,nl;q=0.9,en;q=0.8": "nl",
		"de-DE,de;q=0.9":          English,
		"de,fr;q=0.7,es;q=0.8":    "es",
		"en;q=0.5,fr-CA":          "fr",
		"FR-ca":                   "fr",
	} {
		if got := Match(header); got != want {
			t.Errorf("Match(%q) = %q, want %q", header, got, want)
		}
	}
}

func TestPluralRules(t *testing.T) {
	// Without a translation, the English forms with each language's rule.
	if got := English.N(0, "%d item", "%d items"); got != "0 items" {
		t.Errorf("en 0: %q", got)
	}
	if got := Lang("fr").N(0, "%d item", "%d items"); got != "0 item" {
		t.Errorf("fr 0: %q", got)
	}
	if got := Lang("nl").N(1, "%d item", "%d items"); got != "1 item" {
		t.Errorf("nl 1: %q", got)
	}
}

func TestFallbackAndMsg(t *testing.T) {
	if got := Lang("nl").T("No such message %d", 3); got != "No such message 3" {
		t.Errorf("fallback: %q", got)
	}
	if got := (Msg{}).In("fr"); got != "" {
		t.Errorf("zero Msg: %q", got)
	}
	if got := M("%s and %s", "a", "b").In(English); got != "a and b" {
		t.Errorf("Msg: %q", got)
	}
}
