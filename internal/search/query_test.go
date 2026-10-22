package search

import (
	"reflect"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	q := Parse(`kubernetes "exact phrase" #homelab -#archive site:https://www.GitHub.com/x !fav after:2026-01-01 before:nope foo:bar - `)
	want := Query{
		Terms:   []string{"kubernetes", "before:nope", "foo:bar"},
		Phrases: []string{"exact phrase"},
		Tags:    []string{"homelab"},
		NotTags: []string{"archive"},
		Sites:   []string{"github.com"},
		Fav:     true,
		After:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if !reflect.DeepEqual(q, want) {
		t.Fatalf("got  %+v\nwant %+v", q, want)
	}
}

func TestFTSQuotesInput(t *testing.T) {
	q := Parse(`foo NEAR(bar) "a""b"`)
	if got, want := q.FTS(), `"foo"* "NEAR(bar)"* "a" "b"`; got != want {
		t.Fatalf("FTS() = %q, want %q", got, want)
	}
	if got := Parse("#tag !fav").FTS(); got != "" {
		t.Fatalf("FTS() without text = %q, want empty", got)
	}
}

func TestUnterminatedQuote(t *testing.T) {
	q := Parse(`"open phrase`)
	if !reflect.DeepEqual(q.Phrases, []string{"open phrase"}) {
		t.Fatalf("phrases = %v", q.Phrases)
	}
}
