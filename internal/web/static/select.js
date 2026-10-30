// The list's toolbar acts on the ticked rows. This adds the select all/none
// box and the count, turns off the buttons that don't apply (all of them
// with nothing ticked, Edit unless exactly one is), puts the number in the
// delete confirmation, and lets Shift-click tick a range. Loaded on the list
// page only when the space can be edited. Without it, every button posts
// whatever is ticked and the server says when that won't do.
(function () {
  "use strict";

  var bar = document.querySelector(".bulk-bar");
  if (!bar) return;
  var all = bar.querySelector(".bulk-all");
  var count = bar.querySelector(".bulk-count");
  var title = document.getElementById("confirm-delete-title");
  var last = null; // the box last clicked, for Shift-click

  function boxes() {
    return Array.prototype.slice.call(document.querySelectorAll("li.bookmark .bm-check"));
  }

  // The one|other text on el for n, as select.js can't pick a plural form itself.
  function plural(el, n) {
    return el.getAttribute(n === 1 ? "data-single" : "data-plural").replace(/\d+/, n);
  }

  function update() {
    var list = boxes();
    var n = list.filter(function (b) { return b.checked; }).length;
    all.checked = n > 0 && n === list.length;
    all.indeterminate = n > 0 && n < list.length;
    // Ticked, the box selects none.
    all.title = all.getAttribute(all.checked ? "data-none" : "data-all");
    all.setAttribute("aria-label", all.title);
    count.textContent = n ? plural(count, n) : "";
    bar.classList.toggle("has-selection", n > 0);
    bar.querySelectorAll(".bulk-actions > button").forEach(function (b) {
      b.disabled = b.getAttribute("data-needs") === "one" ? n !== 1 : n === 0;
    });
    if (title) title.textContent = plural(title, n);
  }

  all.setAttribute("data-all", all.title);
  all.hidden = false;

  all.addEventListener("change", function () {
    var on = all.checked; // a click on a partly ticked box ticks them all
    boxes().forEach(function (b) { b.checked = on; });
    last = null;
    update();
  });

  document.addEventListener("click", function (e) {
    var box = e.target.closest && e.target.closest("li.bookmark .bm-check");
    if (!box) return;
    if (e.shiftKey && last && last !== box) {
      var list = boxes();
      var a = list.indexOf(last), b = list.indexOf(box);
      list.slice(Math.min(a, b), Math.max(a, b) + 1).forEach(function (x) { x.checked = box.checked; });
    }
    last = box;
  });

  // keys.js ticks boxes too, and says so with a change event.
  document.addEventListener("change", function (e) {
    if (e.target.classList && e.target.classList.contains("bm-check")) update();
  });

  // A box ticked before this ran, or kept by the browser on back.
  window.addEventListener("pageshow", update);
  update();
})();
