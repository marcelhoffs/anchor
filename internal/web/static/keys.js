// Keyboard shortcuts on the bookmark list: / searches, n adds a bookmark,
// j and k move between rows, and on the current row e edits, f favourites
// and a archives (or unarchives). Enter opens it, as the title link has focus.
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

  // The row's own button or link (on phones they are its ⋯ menu).
  function act(selector) {
    var row = current();
    var el = row && row.querySelector(".bm-actions " + selector);
    if (el) el.click();
  }

  document.addEventListener("keydown", function (e) {
    if (e.ctrlKey || e.altKey || e.metaKey) return;
    var t = e.target;
    if (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName)) {
      if (e.key === "Escape" && t.id === "q") t.blur();
      return;
    }
    var add = document.querySelector('a[href="/bookmarks/new"]');
    switch (e.key) {
      case "/": var q = document.getElementById("q"); if (!q) return; q.focus(); q.select(); break;
      case "n": if (!add) return; add.click(); break;
      case "j": move(1); break;
      case "k": move(-1); break;
      case "e": act('a[href*="/edit"]'); break;
      case "f": act(".bm-fav"); break;
      case "a": act(".bm-archive"); break;
      default: return;
    }
    e.preventDefault();
  });
})();
