package fetch

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/png"
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
		"192.0.0.8": false, "198.18.0.1": false, "198.19.255.1": false, "240.0.0.1": false, "198.20.0.1": true,
		// IPv6 that leads to IPv4: judged by the IPv4 address inside.
		"64:ff9b::a00:1": false, "64:ff9b::5db8:d822": true, "::a00:1": false,
		"2002:a00:1::1": false, "2002:5db8:d822::1": true, "64:ff9b:1::a00:1": false, "2001:0:4136:e378::1": false,
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

func TestSameHost(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"http://ad.nl/x", "https://www.ad.nl/y", true},
		{"https://www.ad.nl/", "https://myprivacy.dpgmedia.nl/consent", false},
		{"https://keep.google.com/", "https://accounts.google.com/signin", false},
		{"https://WWW.Example.com/", "https://example.com:443/", true},
		{"http://127.0.0.1:1/", "http://127.0.0.1:2/", true},
		{"not a url", "not a url", false},
	} {
		if got := SameHost(c.a, c.b); got != c.want {
			t.Errorf("SameHost(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
	for host, want := range map[string]string{
		"cronos.sharepoint.com": "sharepoint.com", "www.bbc.co.uk": "bbc.co.uk", "127.0.0.1": "127.0.0.1", "KEEP.Google.com": "google.com",
	} {
		if got := RegistrableDomain(host); got != want {
			t.Errorf("RegistrableDomain(%q) = %q, want %q", host, got, want)
		}
	}
	for _, c := range []struct {
		from, to string
		want     bool
	}{
		{"https://online.beobank.be/nl/login", "https://www.beobank.be/fr/index.html", true},
		{"https://shop.example.com/", "https://example.com/", true},
		{"https://keep.google.com/", "https://accounts.google.com/signin", false},
		{"https://www.ad.nl/", "https://myprivacy.dpgmedia.nl/consent", false},
	} {
		if got := ToSiteHome(c.from, c.to); got != c.want {
			t.Errorf("ToSiteHome(%q, %q) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestBlank(t *testing.T) {
	pngOf := func(side int, c color.Color) []byte {
		img := image.NewNRGBA(image.Rect(0, 0, side, side))
		img.Set(1, 1, c)
		var b bytes.Buffer
		png.Encode(&b, img)
		return b.Bytes()
	}
	// bitmap is an ICO entry's 32-bit 1×1 bitmap with pixel bgra.
	bitmap := func(bgra ...byte) []byte {
		h := make([]byte, 40)
		binary.LittleEndian.PutUint32(h, 40)
		binary.LittleEndian.PutUint32(h[4:], 1)
		binary.LittleEndian.PutUint32(h[8:], 2) // the mask doubles the height
		binary.LittleEndian.PutUint16(h[12:], 1)
		binary.LittleEndian.PutUint16(h[14:], 32)
		return append(append(h, bgra...), 0, 0, 0, 0) // the mask row
	}
	ico := func(entries ...[]byte) []byte {
		out := []byte{0, 0, 1, 0, byte(len(entries)), 0}
		off := 6 + 16*len(entries)
		for _, e := range entries {
			d := make([]byte, 16)
			binary.LittleEndian.PutUint32(d[8:], uint32(len(e)))
			binary.LittleEndian.PutUint32(d[12:], uint32(off))
			out = append(out, d...)
			off += len(e)
		}
		for _, e := range entries {
			out = append(out, e...)
		}
		return out
	}
	none, red := color.NRGBA{}, color.NRGBA{R: 255, A: 255}
	tooMany := make([][]byte, maxBlankImages+1)
	for i := range tooMany {
		tooMany[i] = bitmap(0, 0, 0, 0)
	}
	for name, c := range map[string]struct {
		blank func([]byte) bool
		data  []byte
		want  bool
	}{
		"clear png":          {blankPNG, pngOf(2, none), true},
		"red png":            {blankPNG, pngOf(2, red), false},
		"huge clear png":     {blankPNG, pngOf(2000, none), false}, // not decoded
		"not a png":          {blankPNG, []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32)), false},
		"clear bitmap ico":   {blankICO, ico(bitmap(0, 0, 0, 0), bitmap(0, 0, 0, 0)), true},
		"one red bitmap ico": {blankICO, ico(bitmap(0, 0, 0, 0), bitmap(0, 0, 255, 255)), false},
		"png ico":            {blankICO, ico(pngOf(2, none), bitmap(0, 0, 0, 0)), true},
		"red png ico":        {blankICO, ico(pngOf(2, red)), false},
		"truncated ico":      {blankICO, ico(bitmap(0, 0, 0, 0))[:30], false},
		"too many images":    {blankICO, ico(tooMany...), false}, // not decoded
	} {
		if got := c.blank(c.data); got != c.want {
			t.Errorf("%s: blank = %v, want %v", name, got, c.want)
		}
	}
}
