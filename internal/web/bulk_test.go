package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/marcelhoffs/anchor/internal/store"
)

// The toolbar's icon-only buttons show what they do on hover.
func TestToolbarTooltips(t *testing.T) {
	c := newEnv(t).admin()
	c.post("/bookmarks", url.Values{"url": {"https://go.dev/"}, "title": {"Go"}})
	_, body, _ := c.get("/")
	bar := body[strings.Index(body, `class="bulk-bar"`):]
	bar = bar[:strings.Index(bar, `<ul class="bookmarks">`)]
	for _, tip := range []string{"Select all", "Edit", "Favourites", "Archive", "Delete"} {
		if !strings.Contains(bar, `title="`+tip+`"`) {
			t.Errorf("no %q tooltip", tip)
		}
	}
	// select.js reads the count's plural forms as plain text; an attribute
	// named like an event handler (data-one) would get JS-quoted.
	if !strings.Contains(bar, `data-single="1 selected" data-plural="2 selected"`) {
		t.Error("count forms are not plain text")
	}
	// A phone moves archive into the More menu.
	if !strings.Contains(bar, `popovertarget="bulk-more"`) || !strings.Contains(bar[strings.Index(bar, `id="bulk-more"`):], `value="archive"`) {
		t.Error("no More menu with archive")
	}
	// With no other space to go to, there's no copy or move.
	if strings.Contains(bar, `value="copy"`) {
		t.Error("copy offered without another space")
	}
}

// The toolbar's Tag menu lists the space's tags as pills, stays on phones,
// and toggles the picked tag on the selection, without JavaScript.
func TestBulkTag(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	c.post("/bookmarks", url.Values{"url": {"https://go.dev"}, "title": {"Go"}})
	if _, body, _ := c.get("/"); strings.Contains(body, `popovertarget="bulk-tag"`) {
		t.Fatal("Tag menu offered without tags")
	}
	e.tags("root", "go")
	c.post("/bookmarks", url.Values{"url": {"https://pkg.go.dev"}, "title": {"Pkg"}, "tags": {"go"}})
	_, body, _ := c.get("/")
	if !strings.Contains(body, `<button type="button" class="icon-btn" popovertarget="bulk-tag"`) {
		t.Fatal("no Tag button, or it's hidden on phones")
	}
	color := store.TagColor("go", "")
	if !strings.Contains(body, `<button class="tag" data-color="`+color+`" data-tag-name="go" aria-pressed="false" form="bulk-form" name="action" value="tag" formaction="/bookmarks/bulk?tag=1">go</button>`) {
		t.Fatal("no pill for the tag")
	}

	sc := personal(e.store, e.userID("root"))
	from := itoa(sc.SpaceID())
	toggle := func() {
		t.Helper()
		v := url.Values{"from": {from}, "action": {"tag"}, "id": {"1", "2"}, "next": {"/"}}
		if code, _, loc := c.post("/bookmarks/bulk?tag=1", v); code != http.StatusSeeOther || loc != "/" {
			t.Fatalf("tag: %d %q", code, loc)
		}
	}
	tagged := func(id int64) bool {
		b, _ := e.store.GetBookmark(context.Background(), sc, id)
		return len(b.Tags) == 1
	}
	toggle() // one had it: both get it
	if !tagged(1) || !tagged(2) {
		t.Fatal("tag not added to all")
	}
	toggle() // both have it: off both
	if tagged(1) || tagged(2) {
		t.Fatal("tag not removed from all")
	}
}

// Delete submits only from inside a confirmation popover; archive and
// unarchive are one click, and archiving offers an undo.
func TestDestructiveActionsNeedConfirmation(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice")
	c.post("/bookmarks", url.Values{"url": {"https://a.example"}, "title": {"Keep"}})
	c.post("/bookmarks", url.Values{"url": {"https://b.example"}, "title": {"Old"}})
	c.post("/bookmarks", url.Values{"url": {"https://c.example"}, "title": {"Older"}})
	from := itoa(personal(e.store, e.userID("alice")).SpaceID())

	// The submit button must be the only one, inside the popover that the
	// trigger button opens.
	confirmed := func(body, id, submit string) bool {
		start := strings.Index(body, `id="`+id+`"`)
		if !strings.Contains(body, `popovertarget="`+id+`"`) || start < 0 || strings.Count(body, submit) != 1 {
			return false
		}
		return strings.Index(body[start:], submit) < strings.Index(body[start:], "</div>\n  </div>")
	}
	deleteButton := `form="bulk-form" name="action" value="delete"`
	_, body, _ := c.get("/")
	if !confirmed(body, "confirm-delete", deleteButton) {
		t.Fatal("list: delete is not behind a confirmation")
	}
	list := body[strings.Index(body, `<ul class="bookmarks">`):strings.Index(body, `</ul>`)]
	if strings.Contains(list, "<form") {
		t.Fatal("list rows have their own forms, want only the toolbar's")
	}
	if !strings.Contains(body, `form="bulk-form" name="action" value="archive"`) {
		t.Fatal("list: archive should be one click")
	}

	// Archiving comes back to the page with an undo, which isn't carried
	// into the page's own links.
	archive := url.Values{"from": {from}, "action": {"archive"}, "id": {"2"}, "next": {"/?q=old"}}
	if _, _, loc := c.post("/bookmarks/bulk", archive); loc != "/?archived=2&q=old" {
		t.Fatalf("archive redirects to %q", loc)
	}
	_, body, _ = c.get("/?archived=2&q=old")
	if !strings.Contains(body, `class="notice undo"`) || !strings.Contains(body, "<strong>Old</strong> moved to the Archive.") ||
		strings.Contains(body, `value="/?archived=2&amp;q=old"`) {
		t.Fatal("no undo after archiving, or it leaks into next")
	}
	if _, body, _ = c.get("/?archived=1"); strings.Contains(body, `class="notice undo"`) {
		t.Fatal("undo offered for a bookmark that isn't archived")
	}
	archive["id"] = []string{"3"}
	c.post("/bookmarks/bulk", archive)
	if _, body, _ = c.get("/?archived=2,3"); !strings.Contains(body, "2 bookmarks moved to the Archive.") ||
		!strings.Contains(body, `name="id" value="2"`) || !strings.Contains(body, `name="action" value="unarchive"`) {
		t.Fatal("no undo for several bookmarks")
	}

	_, body, _ = c.get("/?q=%21archived")
	if !strings.Contains(body, `form="bulk-form" name="action" value="unarchive"`) {
		t.Fatal("archive view: unarchive should be one click")
	}
	if !confirmed(body, "confirm-delete", deleteButton) {
		t.Fatal("archive view: delete is not behind a confirmation")
	}
	_, body, _ = c.get("/bookmarks/1/edit")
	start := strings.Index(body, `id="delete-bookmark"`)
	if start < 0 || !strings.Contains(body, `popovertarget="delete-bookmark"`) ||
		!strings.Contains(body[start:start+strings.Index(body[start:], "</form>")], `action="/bookmarks/1/delete"`) {
		t.Fatal("edit page: delete is not behind a confirmation")
	}
}

// The toolbar's actions on the ticked bookmarks.
func TestBulkActions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.user("alice")
	e.user("bob")
	c := e.login("alice")
	own := personal(e.store, e.userID("alice"))
	bobs := personal(e.store, e.userID("bob"))
	for _, u := range []string{"https://a.example", "https://b.example", "https://c.example"} {
		c.post("/bookmarks", url.Values{"url": {u}})
	}
	theirs, _, _ := e.store.CreateBookmark(ctx, bobs, store.BookmarkInput{URL: "https://bob.example"})
	// bulk posts the toolbar; space is the target of a copy or move.
	bulk := func(action string, space int64, ids ...int64) (int, string) {
		t.Helper()
		form := url.Values{"from": {itoa(own.SpaceID())}, "action": {action}, "next": {"/"}}
		for _, id := range ids {
			form.Add("id", itoa(id))
		}
		code, _, loc := c.post("/bookmarks/bulk?space="+itoa(space), form)
		return code, loc
	}

	// Edit takes exactly one.
	if _, loc := bulk("edit", 0, 2); loc != "/bookmarks/2/edit?next=%2F" {
		t.Fatalf("edit one: %q", loc)
	}
	if _, loc := bulk("edit", 0, 1, 2); loc != "/?done=edit-one" {
		t.Fatalf("edit two: %q", loc)
	}
	if _, body, _ := c.get("/?done=edit-one"); !strings.Contains(body, "Select one bookmark to edit.") {
		t.Fatal("no notice for editing two")
	}
	if _, loc := bulk("archive", 0); loc != "/?done=none" {
		t.Fatalf("nothing ticked: %q", loc)
	}
	if code, _ := bulk("explode", 0, 1); code != http.StatusBadRequest {
		t.Fatalf("unknown action: %d", code)
	}

	// Another user's bookmark is left alone.
	bulk("favourite", 0, 1, 2, theirs.ID)
	if b, _ := e.store.GetBookmark(ctx, own, 2); !b.IsFavourite {
		t.Fatal("not favourited")
	}
	if b, _ := e.store.GetBookmark(ctx, bobs, theirs.ID); b.IsFavourite {
		t.Fatal("favourited bob's bookmark")
	}
	bulk("unfavourite", 0, 1)
	if b, _ := e.store.GetBookmark(ctx, own, 1); b.IsFavourite {
		t.Fatal("not unfavourited")
	}
	bulk("delete", 0, 3, theirs.ID)
	if _, err := e.store.GetBookmark(ctx, own, 3); err != store.ErrNotFound {
		t.Fatal("not deleted")
	}
	if _, err := e.store.GetBookmark(ctx, bobs, theirs.ID); err != nil {
		t.Fatal("deleted bob's bookmark")
	}

	// Copy and move go to spaces alice can add to.
	team, _ := e.store.CreateSpace(ctx, e.userID("alice"), "Team")
	teamID := team.SpaceID()
	_, body, _ := c.get("/")
	for _, action := range []string{"copy", "move"} {
		if !strings.Contains(body, `popovertarget="bulk-`+action+`"`) ||
			!strings.Contains(body, `value="`+action+`" formaction="/bookmarks/bulk?space=`+itoa(teamID)+`"`) {
			t.Fatalf("no %s to the team space", action)
		}
	}
	if code, _ := bulk("copy", bobs.SpaceID(), 1); code != http.StatusNotFound {
		t.Fatalf("copy into bob's space: %d", code)
	}
	_, loc := bulk("copy", teamID, 1, 2)
	if loc != "/?copied=2&skipped=0&to="+itoa(teamID) {
		t.Fatalf("copy: %q", loc)
	}
	if _, body, _ := c.get(loc); !strings.Contains(body, "2 bookmarks copied to <strong>Team</strong>.") {
		t.Fatal("no notice after copying")
	}
	_, loc = bulk("move", teamID, 1)
	if _, body, _ := c.get(loc); !strings.Contains(body, "0 bookmarks moved to <strong>Team</strong>. 1 was skipped: that space has it already.") {
		t.Fatalf("no notice after moving a duplicate (%s)", loc)
	}
	if _, err := e.store.GetBookmark(ctx, own, 1); err != nil {
		t.Fatal("skipped bookmark moved")
	}
}
