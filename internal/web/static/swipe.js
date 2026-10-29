// On touch screens (phones and tablets, either way up), swipe a bookmark row
// left to reveal its actions, which slide in from the right edge as a
// toolbar. Opening one row closes the others; a tap elsewhere or a swipe
// right closes it again. Loaded on the list page only when the space can be
// edited. Without this script (or with a mouse) the row keeps its ⋯ menu or
// hover toolbar.
(function () {
  "use strict";

  if (!window.matchMedia("(pointer: coarse)").matches) return;
  document.documentElement.classList.add("swipe-rows");

  var open = null;  // the row whose actions are showing
  var drag = null;  // {row, width, x, y, base, dx, moved}

  var GAP = 10; // between the open toolbar and the row's right edge

  function width(row) {
    var actions = row.querySelector(".bm-actions");
    return actions ? actions.offsetWidth + GAP : 0;
  }

  function place(row, x, settle) {
    row.classList.toggle("settling", settle);
    row.style.setProperty("--swipe", x + "px");
  }

  function close(row) {
    if (!row) return;
    place(row, 0, true);
    if (open === row) open = null;
  }

  document.addEventListener("touchstart", function (e) {
    drag = null;
    if (e.touches.length !== 1) return;
    var row = e.target.closest("li.bookmark");
    if (open && open !== row) close(open);
    if (!row || !row.querySelector(".bm-actions")) return;
    // Let the buttons be tapped, and the row's menu and confirm popovers, which sit inside it.
    if (e.target.closest(".bm-actions, [popover]")) return;
    var t = e.touches[0];
    drag = { row: row, width: width(row), x: t.clientX, y: t.clientY, base: row === open ? -width(row) : 0, dx: 0, moved: false };
  }, { passive: true });

  document.addEventListener("touchmove", function (e) {
    if (!drag) return;
    var t = e.touches[0];
    var dx = t.clientX - drag.x;
    if (!drag.moved) {
      if (Math.abs(dx) < 8) return;
      if (Math.abs(t.clientY - drag.y) > Math.abs(dx)) { drag = null; return; } // a scroll
      drag.moved = true;
    }
    e.preventDefault(); // no scrolling while swiping
    drag.dx = dx;
    place(drag.row, Math.max(-drag.width, Math.min(0, drag.base + dx)), false);
  }, { passive: false });

  document.addEventListener("touchend", function (e) {
    if (!drag) return;
    var d = drag;
    drag = null;
    if (!d.moved) {
      // A tap on an open row closes it instead of following a link.
      if (d.row === open) { e.preventDefault(); close(d.row); }
      return;
    }
    e.preventDefault(); // no click after a swipe
    if (d.base + d.dx < -d.width / 2) {
      place(d.row, -d.width, true);
      open = d.row;
    } else {
      close(d.row);
    }
  }, { passive: false });

  document.addEventListener("touchcancel", function () {
    if (drag && drag.moved) close(drag.row);
    drag = null;
  });
})();
