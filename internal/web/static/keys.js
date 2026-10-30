// Keyboard shortcuts on the bookmark list: / searches, n adds a bookmark,
// j and k move between rows, x ticks the current row for the toolbar, and
// on the current row alone e edits, f favourites (or unfavourites) and a
// archives (or unarchives) it. Enter opens it, as the title link has focus.
// Keys typed into a field, or with Ctrl, Alt or Cmd, are left alone.
(function () {
  "use strict";

  function rows() {
    return Array.prototype.slice.call(document.querySelectorAll("li.bookmark"));
  }

  function current() {
    return document.activeElement && document.activeElement.closest("li.bookmark");
  }

  function move(by) {
    var all = rows();
    if (!all.length) return;
    var i = all.indexOf(current());
    i = i < 0 ? (by > 0 ? 0 : all.length - 1) : Math.max(0, Math.min(all.length - 1, i + by));
    all[i].querySelector(".bm-title").focus();
  }

  function tick(box, on) {
    box.checked = on;
    box.dispatchEvent(new Event("change", { bubbles: true }));
  }

  // Posts the toolbar's action for the current row only.
  function act(value) {
    var row = current();
    var box = row && row.querySelector(".bm-check");
    var button = document.querySelector('button[form="bulk-form"][value="' + value + '"]');
    if (!box || !button) return;
    rows().forEach(function (r) {
      var b = r.querySelector(".bm-check");
      if (b) b.checked = false;
    });
    tick(box, true);
    button.disabled = false;
    button.form.requestSubmit(button);
  }

  document.addEventListener("keydown", function (e) {
    if (e.ctrlKey || e.altKey || e.metaKey) return;
    var t = e.target;
    if (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName)) {
      if (e.key === "Escape" && t.id === "q") t.blur();
      return;
    }
    var add = document.querySelector('a[href="/bookmarks/new"]');
    var row = current();
    switch (e.key) {
      case "/": var q = document.getElementById("q"); if (!q) return; q.focus(); q.select(); break;
      case "n": if (!add) return; add.click(); break;
      case "j": move(1); break;
      case "k": move(-1); break;
      case "x":
        var box = row && row.querySelector(".bm-check");
        if (!box) return;
        tick(box, !box.checked);
        break;
      case "e": act("edit"); break;
      case "f": act(row && row.hasAttribute("data-fav") ? "unfavourite" : "favourite"); break;
      case "a": act(document.querySelector('button[form="bulk-form"][value="unarchive"]') ? "unarchive" : "archive"); break;
      default: return;
    }
    e.preventDefault();
  });
})();
