// Package importexport reads and writes bookmark files: the Netscape
// bookmark HTML that browsers, linkding, Pocket and Pinboard export, and
// Anchor's own JSON.
package importexport

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	xhtml "golang.org/x/net/html"
)

// Item is one bookmark in a file.
type Item struct {
	URL         string    `json:"url"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Notes       string    `json:"notes"`
	Tags        []string  `json:"tags"`
	Favourite   bool      `json:"favourite"`
	Archived    bool      `json:"archived"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Parse reads a Netscape HTML or Anchor JSON file, whichever data is.
func Parse(data []byte) ([]Item, error) {
	trimmed := bytes.TrimLeftFunc(bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}), unicode.IsSpace) // UTF-8 BOM, whitespace
	if len(trimmed) > 0 && trimmed[0] == '{' {
		return ParseJSON(trimmed)
	}
	if bytes.Contains(bytes.ToLower(data[:min(len(data), 4096)]), []byte("netscape-bookmark-file")) ||
		bytes.Contains(bytes.ToLower(data), []byte("<a ")) {
		return ParseNetscape(bytes.NewReader(data))
	}
	return nil, ErrNotBookmarkFile
}

// MaxItems is the most bookmarks one file may hold. Parsing keeps them all
// in memory, so a file of tiny links (3 million fit in 32 MB) must stop
// early rather than take the server's memory.
const MaxItems = 100_000

// Why Parse refused a file: ErrNotBookmarkFile, ErrNotAnchorJSON (JSON, but
// not an Anchor export), ErrTooManyItems, or a VersionError (an export from
// a newer Anchor).
var (
	ErrNotBookmarkFile = errors.New("not a bookmark file: expected Netscape bookmark HTML or Anchor JSON")
	ErrNotAnchorJSON   = errors.New("not an Anchor JSON export")
	ErrTooManyItems    = fmt.Errorf("more than %d bookmarks in one file", MaxItems)
)

type VersionError struct{ Version int }

func (e VersionError) Error() string {
	return fmt.Sprintf("unsupported Anchor JSON version %d", e.Version)
}

// Browsers' built-in root folders; turning them into tags would tag almost
// everything.
var rootFolders = map[string]bool{
	"bookmarks bar": true, "bookmarks toolbar": true, "bookmarks menu": true, "other bookmarks": true,
	"mobile bookmarks": true, "favorites bar": true, "other favorites": true, "bookmarks": true,
	"unsorted bookmarks": true,
}

// ParseNetscape reads Netscape bookmark HTML. Folders become tags (except the
// browsers' root folders); TAGS (linkding, Pinboard) and tags (Pocket)
// attributes are kept; <DD> text becomes the description.
func ParseNetscape(r io.Reader) ([]Item, error) {
	z := xhtml.NewTokenizer(r)
	var (
		items      []Item
		folders    []string // folder tag per open <DL>, "" for none
		pendingDir string   // folder name seen in <H3>, applied at its <DL>
		inH3, inA  bool
		text       strings.Builder
		cur        *Item // the last <A>, to receive a following <DD>
		inDD       bool
	)
	for {
		tt := z.Next()
		switch tt {
		case xhtml.ErrorToken:
			if z.Err() == io.EOF {
				return items, nil
			}
			return nil, z.Err()
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			attrs := map[string]string{}
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				attrs[string(k)] = string(v)
			}
			switch string(name) {
			case "h3":
				inH3, inDD = true, false
				text.Reset()
			case "dl":
				folders = append(folders, pendingDir)
				pendingDir, inDD = "", false
			case "a":
				if len(items) == MaxItems {
					return nil, ErrTooManyItems
				}
				inA, inDD = true, false
				text.Reset()
				items = append(items, itemFromAttrs(attrs, folders))
				cur = &items[len(items)-1]
			case "dd":
				if cur != nil {
					inDD = true
					text.Reset()
				}
			case "dt":
				if inDD && cur != nil {
					cur.Description = strings.TrimSpace(text.String())
				}
				inDD, cur = false, nil
			}
		case xhtml.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "h3":
				inH3 = false
				pendingDir = folderTag(text.String())
			case "a":
				if inA && cur != nil {
					cur.Title = strings.TrimSpace(text.String())
				}
				inA = false
			case "dl":
				if inDD && cur != nil {
					cur.Description = strings.TrimSpace(text.String())
				}
				inDD, cur = false, nil
				if len(folders) > 0 {
					folders = folders[:len(folders)-1]
				}
			}
		case xhtml.TextToken:
			if inH3 || inA || inDD {
				text.Write(z.Text())
			}
		}
	}
}

func itemFromAttrs(attrs map[string]string, folders []string) Item {
	it := Item{URL: strings.TrimSpace(attrs["href"])}
	for _, f := range folders {
		if f != "" {
			it.Tags = append(it.Tags, f)
		}
	}
	for _, key := range []string{"tags", "tag"} {
		for _, t := range strings.Split(attrs[key], ",") {
			if t = strings.TrimSpace(t); t != "" {
				it.Tags = append(it.Tags, folderTag(t))
			}
		}
	}
	// A folder is often in TAGS too (Anchor's own export).
	seen := map[string]bool{}
	it.Tags = slices.DeleteFunc(it.Tags, func(t string) bool {
		dup := seen[t]
		seen[t] = true
		return dup
	})
	it.CreatedAt = unixAttr(attrs["add_date"], attrs["time_added"])
	it.UpdatedAt = unixAttr(attrs["last_modified"])
	return it
}

// folderTag turns a folder name into a tag name. Tags may contain spaces but
// not commas or double quotes, so those become spaces.
func folderTag(name string) string {
	name = strings.TrimSpace(name)
	if rootFolders[strings.ToLower(name)] {
		return ""
	}
	return strings.Join(strings.FieldsFunc(name, func(r rune) bool { return unicode.IsSpace(r) || r == ',' || r == '"' }), " ")
}

// unixAttr parses the first usable Unix timestamp; Firefox writes seconds,
// some exporters microseconds.
func unixAttr(vals ...string) time.Time {
	for _, v := range vals {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		for n > 1e11 { // milli- or microseconds
			n /= 1000
		}
		return time.Unix(n, 0).UTC()
	}
	return time.Time{}
}

// Anchor's JSON export is {"format": jsonFormat, "version": 1,
// "exported_at": …, "bookmarks": [Item…]}.
const jsonFormat = "anchor-bookmarks"

// ParseJSON reads an Anchor JSON export. Bookmarks are decoded one at a
// time, so a file with too many stops at MaxItems.
func ParseJSON(data []byte) ([]Item, error) {
	invalid := func(err error) error { return fmt.Errorf("%w: invalid JSON: %v", ErrNotAnchorJSON, err) }
	dec := json.NewDecoder(bytes.NewReader(data))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, invalid(cmp.Or(err, errors.New("not an object")))
	}
	var (
		format  string
		version int
		items   []Item
	)
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, invalid(err)
		}
		switch key {
		case "format":
			err = dec.Decode(&format)
		case "version":
			err = dec.Decode(&version)
		case "bookmarks":
			if t, terr := dec.Token(); terr != nil || t != json.Delim('[') {
				return nil, invalid(cmp.Or(terr, errors.New("bookmarks is not a list")))
			}
			for err == nil && dec.More() {
				if len(items) == MaxItems {
					return nil, ErrTooManyItems
				}
				var it Item
				if err = dec.Decode(&it); err == nil {
					items = append(items, it)
				}
			}
			if err == nil {
				_, err = dec.Token() // ]
			}
		default:
			var skip json.RawMessage
			err = dec.Decode(&skip)
		}
		if err != nil {
			return nil, invalid(err)
		}
	}
	if format != jsonFormat {
		return nil, ErrNotAnchorJSON
	}
	if version != 1 {
		return nil, VersionError{version}
	}
	return items, nil
}

// Writer streams bookmarks into an export file.
type Writer interface {
	Write(Item) error
	Close() error
}

type netscapeWriter struct {
	w       io.Writer
	folders map[string][]Item // by tag
	root    []Item            // untagged
}

// NewNetscapeWriter writes Netscape bookmark HTML, which every browser and
// bookmark manager can import. Each tag becomes a folder, by name, holding
// its bookmarks: one with several tags is in each of their folders (an
// Anchor import merges the copies). Untagged ones follow at the top level.
// Tags also go in the TAGS attribute, for linkding and Pinboard. The
// folders are only known at the end, so nothing is written before Close.
func NewNetscapeWriter(w io.Writer) (Writer, error) {
	_, err := io.WriteString(w, `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<!-- This is an automatically generated file. It will be read and overwritten. DO NOT EDIT! -->
<META HTTP-EQUIV="Content-Type" CONTENT="text/html; charset=UTF-8">
<TITLE>Bookmarks</TITLE>
<H1>Bookmarks</H1>
<DL><p>
`)
	return &netscapeWriter{w: w, folders: map[string][]Item{}}, err
}

func (n *netscapeWriter) Write(it Item) error {
	if len(it.Tags) == 0 {
		n.root = append(n.root, it)
	}
	for _, t := range it.Tags {
		n.folders[t] = append(n.folders[t], it)
	}
	return nil
}

func (n *netscapeWriter) Close() error {
	names := make([]string, 0, len(n.folders))
	for t := range n.folders {
		names = append(names, t)
	}
	slices.SortFunc(names, func(a, b string) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a), strings.ToLower(b)), cmp.Compare(a, b))
	})
	var b strings.Builder
	for _, t := range names {
		fmt.Fprintf(&b, "<DT><H3>%s</H3>\n<DL><p>\n", html.EscapeString(t))
		for _, it := range n.folders[t] {
			writeNetscapeItem(&b, it)
		}
		b.WriteString("</DL><p>\n")
	}
	for _, it := range n.root {
		writeNetscapeItem(&b, it)
	}
	b.WriteString("</DL><p>\n")
	_, err := io.WriteString(n.w, b.String())
	return err
}

func writeNetscapeItem(b *strings.Builder, it Item) {
	fmt.Fprintf(b, `<DT><A HREF="%s"`, html.EscapeString(it.URL))
	if !it.CreatedAt.IsZero() {
		fmt.Fprintf(b, ` ADD_DATE="%d"`, it.CreatedAt.Unix())
	}
	if !it.UpdatedAt.IsZero() {
		fmt.Fprintf(b, ` LAST_MODIFIED="%d"`, it.UpdatedAt.Unix())
	}
	if len(it.Tags) > 0 {
		fmt.Fprintf(b, ` TAGS="%s"`, html.EscapeString(strings.Join(it.Tags, ",")))
	}
	fmt.Fprintf(b, ">%s</A>\n", html.EscapeString(cmp.Or(it.Title, it.URL)))
	if it.Description != "" {
		fmt.Fprintf(b, "<DD>%s\n", html.EscapeString(it.Description))
	}
}

type jsonWriter struct {
	w     io.Writer
	enc   *json.Encoder
	first bool
}

// NewJSONWriter writes Anchor JSON, which keeps notes and flags too.
func NewJSONWriter(w io.Writer, exportedAt time.Time) (Writer, error) {
	head, _ := json.Marshal(struct {
		Format     string    `json:"format"`
		Version    int       `json:"version"`
		ExportedAt time.Time `json:"exported_at"`
	}{jsonFormat, 1, exportedAt.UTC()})
	_, err := w.Write(append(head[:len(head)-1], []byte(`,"bookmarks":[`+"\n")...))
	return &jsonWriter{w: w, enc: json.NewEncoder(w), first: true}, err
}

func (j *jsonWriter) Write(it Item) error {
	if it.Tags == nil {
		it.Tags = []string{}
	}
	if !j.first {
		if _, err := io.WriteString(j.w, ","); err != nil {
			return err
		}
	}
	j.first = false
	return j.enc.Encode(it) // Encode adds the newline
}

func (j *jsonWriter) Close() error {
	_, err := io.WriteString(j.w, "]}\n")
	return err
}
