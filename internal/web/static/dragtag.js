// Drag a bookmark's icon onto a tag in the sidebar to tag it, or onto
// Favourites or Archive to favourite or archive it. Loaded on the list page
// only when the space can be edited. Markup contract:
//   <… data-drag-bookmark="<id>" draggable="true">  the icon, in li.bookmark
//   <a data-tag-drop="<tag id>"><span class="count">  a tag in the sidebar
//   <a data-flag-drop="favourite|archived">          a view in the sidebar
// POST /bookmarks/<id>/tags answers {tags: [{name, url}], count}; the row's
// tags and the tag's count are redrawn from it. A flag changes the row's
// star, menu, the counts and whether it's listed at all, so the page
// reloads. Without this script the icons just don't drag; the edit form
// and the row's buttons still do the same.
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
    var id = encodeURIComponent(e.dataTransfer.getData(TYPE));
    var flag = tag.getAttribute("data-flag-drop");
    if (flag) {
      setFlag(id, flag);
      return;
    }
    var row = dragged.closest("li.bookmark");
    var tagID = tag.getAttribute("data-tag-drop");
    var body = new URLSearchParams({ tag: tagID });
    fetch("/bookmarks/" + id + "/tags", {
      method: "POST",
      credentials: "same-origin",
      headers: { "X-CSRF-Token": csrf },
      body: body
    }).then(function (res) {
      if (!res.ok) throw new Error(res.status);
      return res.json();
    }).then(function (data) {
      redrawTags(row, data.tags);
      // The sidebar and the phone drawer both list the tag.
      document.querySelectorAll('[data-tag-drop="' + tagID + '"] .count').forEach(function (c) {
        c.textContent = data.count;
      });
    }).catch(function () {
      window.location.reload(); // show what the server has
    });
  });

  // setFlag turns a flag on. The redirect the server answers with isn't
  // followed: the reload shows the result, or what the server has.
  function setFlag(id, flag) {
    fetch("/bookmarks/" + id + "/flag", {
      method: "POST",
      credentials: "same-origin",
      redirect: "manual",
      headers: { "X-CSRF-Token": csrf },
      body: new URLSearchParams({ flag: flag, on: "1", next: "/" })
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
      a.textContent = t.name;
      box.appendChild(a);
    });
    row.classList.remove("just-tagged");
    void row.offsetWidth; // restart the highlight
    row.classList.add("just-tagged");
  }
})();
