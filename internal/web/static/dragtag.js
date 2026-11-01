// Drag a bookmark's icon onto a tag in the sidebar to tag it, or onto
// Favourites or Archive to favourite or archive it. Loaded on the list page
// only when the space can be edited. Markup contract:
//   <… data-drag-bookmark="<id>" draggable="true">  the icon, in li.bookmark
//   <a data-tag-drop="<tag id>"><span class="count">  a tag in the sidebar
//   <a data-flag-drop="favourite|archived">          a view in the sidebar
// POST /bookmarks/<id>/tags answers {tags: [{name, url}], count}; the row's
// tags and the tag's count are redrawn from it. A flag changes the row's
// star, the counts and whether it's listed at all, so the page
// reloads. Dragging a ticked row (select.js) does this to every ticked row:
// a tag request per row, and the flag through the toolbar's form. Without
// this script the icons just don't drag; the edit form and the list's
// toolbar still do the same.
(function () {
  "use strict";

  var TYPE = "application/x-anchor-bookmark";
  var csrf = (document.querySelector('meta[name="csrf-token"]') || {}).content || "";
  var dragged = null; // the icon being dragged

  function carriesBookmark(e) {
    return dragged && e.dataTransfer && Array.prototype.indexOf.call(e.dataTransfer.types, TYPE) >= 0;
  }

  document.addEventListener("dragstart", function (e) {
    var icon = e.target.closest && e.target.closest("[data-drag-bookmark]");
    if (!icon) return;
    dragged = icon;
    e.dataTransfer.setData(TYPE, icon.getAttribute("data-drag-bookmark"));
    e.dataTransfer.effectAllowed = "copy";
    document.body.classList.add("dragging-bookmark");
  });

  document.addEventListener("dragend", function () {
    dragged = null;
    document.body.classList.remove("dragging-bookmark");
    document.querySelectorAll(".drop-over").forEach(function (el) { el.classList.remove("drop-over"); });
  });

  function target(e) {
    return e.target.closest && e.target.closest("[data-tag-drop], [data-flag-drop]");
  }

  document.addEventListener("dragover", function (e) {
    var tag = target(e);
    if (!tag || !carriesBookmark(e)) return;
    e.preventDefault(); // allows the drop
    e.dataTransfer.dropEffect = "copy";
    tag.classList.add("drop-over");
  });

  document.addEventListener("dragleave", function (e) {
    var tag = target(e);
    if (tag && !tag.contains(e.relatedTarget)) tag.classList.remove("drop-over");
  });

  document.addEventListener("drop", function (e) {
    var tag = target(e);
    if (!tag || !carriesBookmark(e)) return;
    e.preventDefault(); // don't follow the tag's link
    tag.classList.remove("drop-over");
    var rows = dropped(dragged.closest("li.bookmark"));
    var flag = tag.getAttribute("data-flag-drop");
    if (flag) {
      setFlag(rows, flag);
      return;
    }
    var tagID = tag.getAttribute("data-tag-drop");
    Promise.all(rows.map(function (row) {
      return fetch("/bookmarks/" + encodeURIComponent(bookmarkID(row)) + "/tags", {
        method: "POST",
        credentials: "same-origin",
        headers: { "X-CSRF-Token": csrf },
        body: new URLSearchParams({ tag: tagID })
      }).then(function (res) {
        if (!res.ok) throw new Error(res.status);
        return res.json();
      }).then(function (data) {
        redrawTags(row, data.tags);
        return data.count;
      });
    })).then(function (counts) {
      // The last request to finish saw them all tagged.
      var count = Math.max.apply(null, counts);
      // The sidebar and the phone drawer both list the tag.
      document.querySelectorAll('[data-tag-drop="' + tagID + '"] .count').forEach(function (c) {
        c.textContent = count;
      });
    }).catch(function () {
      window.location.reload(); // show what the server has
    });
  });

  function bookmarkID(row) {
    return row.querySelector("[data-drag-bookmark]").getAttribute("data-drag-bookmark");
  }

  // dropped is what a drop acts on: every ticked row when the dragged one
  // is ticked, else just the dragged one.
  function dropped(row) {
    var box = row.querySelector(".bm-check");
    if (!box || !box.checked) return [row];
    return Array.prototype.slice.call(document.querySelectorAll("li.bookmark")).filter(function (r) {
      var b = r.querySelector(".bm-check");
      return b && b.checked;
    });
  }

  // setFlag turns a flag on, through the toolbar's form. The redirect the
  // server answers with isn't followed: the reload shows the result, or
  // what the server has.
  function setFlag(rows, flag) {
    var form = document.getElementById("bulk-form");
    var body = new URLSearchParams({ action: flag === "archived" ? "archive" : flag });
    form.querySelectorAll("input[type=hidden]").forEach(function (input) { body.set(input.name, input.value); });
    rows.forEach(function (row) { body.append("id", bookmarkID(row)); });
    fetch(form.action, {
      method: "POST",
      credentials: "same-origin",
      redirect: "manual",
      headers: { "X-CSRF-Token": csrf },
      body: body
    }).finally(function () {
      window.location.reload();
    });
  }

  // redrawTags replaces a row's tag links, beside its title.
  function redrawTags(row, tags) {
    var head = row && row.querySelector(".bm-head");
    if (!head) return;
    var box = head.querySelector(".bm-tags");
    if (!box) {
      box = document.createElement("span");
      box.className = "bm-tags";
      head.appendChild(box);
    }
    box.textContent = "";
    tags.forEach(function (t) {
      var a = document.createElement("a");
      a.className = "tag";
      a.href = t.url;
      a.dataset.color = t.color;
      a.dataset.tagName = t.name;
      a.textContent = t.name;
      box.appendChild(a);
    });
    row.classList.remove("just-tagged");
    void row.offsetWidth; // restart the highlight
    row.classList.add("just-tagged");
  }
})();
