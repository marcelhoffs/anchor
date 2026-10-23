package ratelimit

import (
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := New(3, time.Minute)
	l.now = func() time.Time { return now }

	for i := range 3 {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("event %d refused", i)
		}
	}
	if ok, retry := l.Allow("a"); ok || retry != time.Minute {
		t.Fatalf("4th event: ok=%v retry=%v", ok, retry)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("keys must be independent")
	}
	now = now.Add(time.Minute)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("new window refused")
	}

	for range 3 {
		l.Allow("c")
	}
	l.Reset("c")
	if ok, _ := l.Allow("c"); !ok {
		t.Fatal("still limited after Reset")
	}

	now = now.Add(2 * time.Minute)
	l.Sweep()
	if len(l.keys) != 0 {
		t.Fatalf("sweep left %d keys", len(l.keys))
	}
}
