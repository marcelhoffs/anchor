package web

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestManifest(t *testing.T) {
	e := newEnv(t)
	anon := e.client()
	for accent := range iconColours {
		for _, lang := range []string{"en", "nl", "xx"} {
			code, body, _ := anon.get("/app.webmanifest?accent=" + accent + "&lang=" + lang)
			if code != http.StatusOK {
				t.Fatalf("%s %s: %d", accent, lang, code)
			}
			var m struct {
				ID          string         `json:"id"`
				StartURL    string         `json:"start_url"`
				Scope       string         `json:"scope"`
				Display     string         `json:"display"`
				Name        string         `json:"name"`
				ShortName   string         `json:"short_name"`
				Description string         `json:"description"`
				ThemeColor  string         `json:"theme_color"`
				Icons       []manifestIcon `json:"icons"`
				ShareTarget struct {
					Action string `json:"action"`
					Method string `json:"method"`
				} `json:"share_target"`
			}
			if err := json.Unmarshal([]byte(body), &m); err != nil {
				t.Fatalf("%s %s: %v\n%s", accent, lang, err, body)
			}
			if m.ID != "/" || m.StartURL != "/" || m.Scope != "/" || m.Display != "standalone" || m.Name != "Anchor" ||
				m.ShortName == "" || m.Description == "" || m.ThemeColor != iconColours[accent][0] {
				t.Fatalf("%s %s: %+v", accent, lang, m)
			}
			if m.ShareTarget.Action != "/bookmarks/new" || m.ShareTarget.Method != "GET" {
				t.Fatalf("share target: %+v", m.ShareTarget)
			}
			var maskable, png512 bool
			for _, ic := range m.Icons {
				maskable = maskable || ic.Purpose == "maskable"
				png512 = png512 || ic.Sizes == "512x512" && ic.Type == "image/png"
				if code, _, _ := anon.get(ic.Src); code != http.StatusOK {
					t.Fatalf("icon %s: %d", ic.Src, code)
				}
			}
			if !maskable || !png512 {
				t.Fatalf("%s: icons %+v", accent, m.Icons)
			}
			if lang == "nl" && !strings.Contains(m.Description, "bladwijzers") {
				t.Fatalf("description not translated: %q", m.Description)
			}
		}
	}
	if code, _, _ := anon.get("/app.webmanifest?accent=pink"); code != http.StatusNotFound {
		t.Fatalf("unknown accent: %d", code)
	}
}

// Adding an accent can't forget its PNG icons: run static/icons/generate.sh.
func TestEveryAccentHasItsIcons(t *testing.T) {
	for accent := range iconColours {
		for _, f := range []string{"icon-" + accent + "-192", "icon-" + accent + "-512",
			"maskable-" + accent + "-192", "maskable-" + accent + "-512", "apple-touch-" + accent} {
			if _, err := fs.Stat(assets, "static/icons/"+f+".png"); err != nil {
				t.Errorf("missing static/icons/%s.png", f)
			}
		}
		if _, ok := themeColours[accent]; !ok {
			t.Errorf("%s has no theme colours", accent)
		}
	}
}

func TestServiceWorker(t *testing.T) {
	e := newEnv(t)
	res, err := http.Get(e.srv.URL + "/sw.js")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	body := string(b)
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/javascript") ||
		res.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("sw.js: %d %v", res.StatusCode, res.Header)
	}
	if !strings.Contains(body, `"`+assetVersion+`"`) || strings.Contains(body, "__") {
		t.Fatalf("sw.js not filled in:\n%s", body)
	}
	if !strings.Contains(body, `"/offline"`) || !strings.Contains(body, assetURL("style.css")) {
		t.Fatal("sw.js doesn't cache the offline page and the stylesheet")
	}
	// Every file it caches exists, or installing the worker fails.
	c := e.client()
	for _, u := range precache {
		if code, _, _ := c.get(u); code != http.StatusOK {
			t.Errorf("precached %s: %d", u, code)
		}
	}
}

func TestOfflinePageHasNoUserData(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	code, body, _ := c.get("/offline")
	if code != http.StatusOK || !strings.Contains(body, "Anchor needs a connection") {
		t.Fatalf("offline page: %d", code)
	}
	if strings.Contains(body, "root") || strings.Contains(body, `class="top"`) {
		t.Fatal("offline page shows the signed-in user")
	}
}

func TestHeadTags(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	_, body, _ := c.get("/")
	for _, want := range []string{
		`<link rel="manifest" href="/app.webmanifest?accent=indigo&amp;lang=en&amp;v=` + assetVersion + `">`,
		`<link rel="apple-touch-icon" href="/static/icons/apple-touch-indigo.png?v=`,
		`<meta name="theme-color" content="#4f46e5" media="(prefers-color-scheme: light)">`,
		`<meta name="theme-color" content="#8b8cf8" media="(prefers-color-scheme: dark)">`,
		`viewport-fit=cover`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	c.post("/settings/appearance", url.Values{"accent": {"rose"}, "theme": {"dark"}})
	if _, body, _ := c.get("/"); !strings.Contains(body, `<meta name="theme-color" content="#fb7185">`) {
		t.Error("dark theme should have one dark theme colour")
	}
}

func TestShareTarget(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	for _, tc := range []struct{ query, url, desc string }{
		{"text=" + url.QueryEscape("Look at this https://example.com/a?b=1"), "https://example.com/a?b=1", "Look at this"},
		{"url=" + url.QueryEscape("https://example.com/b") + "&title=Bee&text=Bee", "https://example.com/b", ""},
		{"text=" + url.QueryEscape("just some words"), "", "just some words"},
	} {
		code, body, _ := c.get("/bookmarks/new?" + tc.query)
		if code != http.StatusOK {
			t.Fatalf("%s: %d", tc.query, code)
		}
		if !strings.Contains(body, `name="url" value="`+strings.ReplaceAll(tc.url, "&", "&amp;")+`"`) {
			t.Errorf("%s: url isn't %q", tc.query, tc.url)
		}
		if !strings.Contains(body, `maxlength="10000">`+tc.desc+`</textarea>`) {
			t.Errorf("%s: description isn't %q", tc.query, tc.desc)
		}
	}
	// A link that's already saved goes to its edit page.
	c.post("/bookmarks", url.Values{"url": {"https://example.com/saved"}})
	if code, _, loc := c.get("/bookmarks/new?text=" + url.QueryEscape("see https://example.com/saved")); code != http.StatusSeeOther || !strings.HasSuffix(loc, "?dup=1") {
		t.Fatalf("shared saved link: %d %q", code, loc)
	}
}
