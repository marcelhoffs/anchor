package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestParseHead(t *testing.T) {
	base, _ := url.Parse("https://example.com/blog/post")
	var m Meta
	parseHead(strings.NewReader(`<!doctype html><html><head>
		<meta property="og:title" content="OG title">
		<title>
		  The   real
		  title </title>
		<meta property="og:description" content="OG description">
		<link rel="apple-touch-icon" href="/apple.png">
		<link rel="icon" type="image/svg+xml" href="/icon.svg">
		<link rel="shortcut icon" href="favicon.png">
		<link rel="icon" href="data:image/png;base64,AAAA">
		</head><body><title>not this</title><meta name="description" content="not this"></body></html>`), base, &m)
	if m.Title != "The real title" || m.Description != "OG description" {
		t.Fatalf("meta = %+v", m)
	}
	if want := []string{"https://example.com/blog/favicon.png", "https://example.com/apple.png"}; !slices.Equal(m.Icons, want) {
		t.Fatalf("icons = %v, want %v", m.Icons, want)
	}

	m = Meta{}
	parseHead(strings.NewReader(`<head><meta property="og:title" content="Only OG"><meta name="description" content="Plain"></head>`), base, &m)
	if m.Title != "Only OG" || m.Description != "Plain" {
		t.Fatalf("fallbacks = %+v", m)
	}
}

func TestClean(t *testing.T) {
	if got := clean(strings.Repeat("é", 10), 5); got != "éééé…" {
		t.Fatalf("clean = %q", got)
	}
}

func TestPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"93.184.216.34": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "192.168.1.1": false, "172.16.0.1": false,
		"169.254.169.254": false, "100.100.1.1": false, "0.1.2.3": false, "::1": false,
		"fd00::1": false, "fe80::1": false, "::ffff:127.0.0.1": false, "224.0.0.1": false,
	} {
		if got := public(netip.MustParseAddr(addr)); got != want {
			t.Errorf("public(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestPageAndIcon(t *testing.T) {
	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			http.Redirect(w, r, "/page", http.StatusFound)
		case "/page":
			w.Header().Set("Content-Type", "text/html; charset=iso-8859-1")
			w.Write([]byte("<title>Caf\xe9</title><link rel=icon href=/i.png>"))
		case "/i.png":
			w.Write([]byte(png))
		case "/fake.png":
			w.Write([]byte("<svg onload=alert(1)>"))
		case "/doc.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			w.Write([]byte("%PDF-1.7"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	// 127.0.0.1 is refused unless private addresses are allowed.
	if _, err := New(false).Page(ctx, srv.URL); !errors.Is(err, ErrBlocked) {
		t.Fatalf("loopback not blocked: %v", err)
	}
	c := New(true)
	m, err := c.Page(ctx, srv.URL)
	if err != nil || m.Title != "Café" || m.FinalURL != srv.URL+"/page" || !slices.Equal(m.Icons, []string{srv.URL + "/i.png"}) {
		t.Fatalf("page = %+v, %v", m, err)
	}
	if m, err := c.Page(ctx, srv.URL+"/doc.pdf"); err != nil || m.Title != "" {
		t.Fatalf("pdf = %+v, %v", m, err)
	}
	if _, err := c.Page(ctx, srv.URL+"/missing"); err == nil {
		t.Fatal("404 page should fail")
	}
	if mime, data, err := c.Icon(ctx, srv.URL+"/i.png"); err != nil || mime != "image/png" || string(data) != png {
		t.Fatalf("icon = %q, %v", mime, err)
	}
	if _, _, err := c.Icon(ctx, srv.URL+"/fake.png"); err == nil {
		t.Fatal("non-image accepted as an icon")
	}
	if _, err := c.Page(ctx, "file:///etc/passwd"); err == nil {
		t.Fatal("non-http URL fetched")
	}
}

func TestSameSite(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"https://www.ad.nl/", "https://statics.ad.nl/img/favicon.ico", true},
		{"http://ad.nl/x", "https://www.ad.nl/y", true},
		{"https://www.ad.nl/", "https://myprivacy.dpgmedia.nl/consent", false},
		{"https://www.bbc.co.uk/", "https://news.bbc.co.uk/", true},
		{"https://a.co.uk/", "https://b.co.uk/", false},
		{"http://127.0.0.1:1/", "http://127.0.0.1:2/", true},
		{"http://127.0.0.1/", "http://localhost/", false},
		{"not a url", "not a url", false},
	} {
		if got := SameSite(c.a, c.b); got != c.want {
			t.Errorf("SameSite(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
