package importexport

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Shaped like a Chrome export: nested folders, unclosed <DT>/<p>, a root
// "Bookmarks bar" folder, and a <DD> description (as Firefox and linkding
// write).
const chrome = `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<META HTTP-EQUIV="Content-Type" CONTENT="text/html; charset=UTF-8">
<TITLE>Bookmarks</TITLE>
<H1>Bookmarks</H1>
<DL><p>
    <DT><H3 ADD_DATE="1700000000" PERSONAL_TOOLBAR_FOLDER="true">Bookmarks bar</H3>
    <DL><p>
        <DT><A HREF="https://go.dev/" ADD_DATE="1700000001">Go &amp; friends</A>
        <DD>The Go website
        <DT><H3>Home Lab</H3>
        <DL><p>
            <DT><A HREF="https://pve.proxmox.com/" ADD_DATE="1700000002000000" TAGS="proxmox,virtualisation">Proxmox</A>
        </DL><p>
    </DL><p>
    <DT><A HREF="https://example.org/" TIME_ADDED="1700000003" tags="pocket">Pocket item</A>
</DL><p>
`

func TestParseNetscape(t *testing.T) {
	items, err := Parse([]byte(chrome))
	if err != nil {
		t.Fatal(err)
	}
	want := []Item{
		{URL: "https://go.dev/", Title: "Go & friends", Description: "The Go website", CreatedAt: time.Unix(1700000001, 0).UTC()},
		{URL: "https://pve.proxmox.com/", Title: "Proxmox", Tags: []string{"Home Lab", "proxmox", "virtualisation"}, CreatedAt: time.Unix(1700000002, 0).UTC()},
		{URL: "https://example.org/", Title: "Pocket item", Tags: []string{"pocket"}, CreatedAt: time.Unix(1700000003, 0).UTC()},
	}
	if !reflect.DeepEqual(items, want) {
		t.Fatalf("got  %+v\nwant %+v", items, want)
	}
}

func TestRoundTrip(t *testing.T) {
	in := []Item{
		{URL: "https://a.example/?q=1&b=<2>", Title: `Quote " and <tag>`, Description: "desc", Notes: "notes\nline 2",
			Tags: []string{"x", "y"}, Favourite: true, CreatedAt: time.Unix(1700000000, 0).UTC(), UpdatedAt: time.Unix(1700000100, 0).UTC()},
		{URL: "https://b.example/", Archived: true, CreatedAt: time.Unix(1700000200, 0).UTC()},
	}

	var buf bytes.Buffer
	w, _ := NewJSONWriter(&buf, time.Unix(1700001000, 0))
	for _, it := range in {
		w.Write(it)
	}
	w.Close()
	got, err := Parse(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	in[1].Tags = []string{}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("JSON round trip:\ngot  %+v\nwant %+v", got, in)
	}

	buf.Reset()
	w, _ = NewNetscapeWriter(&buf)
	for _, it := range in {
		w.Write(it)
	}
	w.Close()
	got, err = Parse(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	// HTML keeps URL, title, description, tags and dates, not notes or flags.
	// Each tag is a folder holding its bookmarks, so the first is in both x
	// and y; untagged ones come last, at the top.
	if len(got) != 3 || got[0].URL != in[0].URL || got[0].Title != in[0].Title || got[0].Description != "desc" ||
		!reflect.DeepEqual(got[0].Tags, []string{"x", "y"}) || !got[0].CreatedAt.Equal(in[0].CreatedAt) ||
		!got[0].UpdatedAt.Equal(in[0].UpdatedAt) || got[1].URL != in[0].URL || !reflect.DeepEqual(got[1].Tags, []string{"y", "x"}) ||
		got[2].Title != "https://b.example/" || len(got[2].Tags) != 0 {
		t.Fatalf("HTML round trip: %+v", got)
	}
}

// What a browser sees: one folder per tag, by name, and untagged bookmarks
// at the top level.
func TestNetscapeFolders(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewNetscapeWriter(&buf)
	for _, it := range []Item{
		{URL: "https://go.dev/", Title: "Go", Tags: []string{"lang", "Dev & ops"}},
		{URL: "https://plain.example/", Title: "Plain"},
		{URL: "https://zig.example/", Title: "Zig", Tags: []string{"lang"}},
	} {
		w.Write(it)
	}
	w.Close()
	want := `<DL><p>
<DT><H3>Dev &amp; ops</H3>
<DL><p>
<DT><A HREF="https://go.dev/" TAGS="lang,Dev &amp; ops">Go</A>
</DL><p>
<DT><H3>lang</H3>
<DL><p>
<DT><A HREF="https://go.dev/" TAGS="lang,Dev &amp; ops">Go</A>
<DT><A HREF="https://zig.example/" TAGS="lang">Zig</A>
</DL><p>
<DT><A HREF="https://plain.example/">Plain</A>
</DL><p>
`
	if got := buf.String(); !strings.HasSuffix(got, want) {
		t.Fatalf("export:\n%s", got)
	}
}

func TestParseRejects(t *testing.T) {
	for _, bad := range []string{"", "hello", `{"format":"other"}`, `{"format":"anchor-bookmarks","version":9}`} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}
