package websearch

import "testing"

func TestURL(t *testing.T) {
	for _, c := range []struct {
		engine, q, locale string
		safe              bool
		want              string
	}{
		{"duckduckgo", "go generics", "nl-BE", true, "https://duckduckgo.com/?kl=be-nl&kp=1&q=go+generics"},
		{"duckduckgo", "x", "en-GB", false, "https://duckduckgo.com/?kl=uk-en&kp=-2&q=x"},
		{"duckduckgo", "x", "nb-NO", false, "https://duckduckgo.com/?kl=no-no&kp=-2&q=x"},
		{"duckduckgo", "x", "", true, "https://duckduckgo.com/?kp=1&q=x"},
		{"ecosia", "a&b", "fr-FR", true, "https://www.ecosia.org/search?q=a%26b"},
		{"google", "x", "fr-BE", true, "https://www.google.com/search?hl=fr&gl=BE&safe=active&q=x"},
		{"google", "x", "", false, "https://www.google.com/search?safe=off&q=x"},
		{"qwant", "x", "de-AT", true, "https://www.qwant.com/?locale=de_AT&s=2&q=x"},
		{"qwant", "", "", false, "https://www.qwant.com/?s=0&q="},
		{"bing", "x", "", true, ""},
	} {
		if got := URL(c.engine, c.q, c.locale, c.safe); got != c.want {
			t.Errorf("URL(%q, %q, %q, %v) = %q, want %q", c.engine, c.q, c.locale, c.safe, got, c.want)
		}
	}
}

func TestMatch(t *testing.T) {
	for header, want := range map[string]string{
		"nl-BE,nl;q=0.9,en;q=0.8": "nl-BE",
		"nl":                      "nl-NL", // the likely region
		"en-GB":                   "en-GB",
		"de-LU":                   "de-DE", // a region not listed: the language's first
		"ja,fr;q=0.5":             "fr-FR",
		"ja":                      "",
		"":                        "",
	} {
		if got := Match(header); got != want {
			t.Errorf("Match(%q) = %q, want %q", header, got, want)
		}
	}
}
