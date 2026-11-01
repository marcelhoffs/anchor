// The list's toolbar acts on the ticked rows. This adds the select all/none
// button and the count, turns off the buttons that don't apply (all of them
// with nothing ticked, Edit unless exactly one is), puts the number in the
// delete confirmation, presses the tags the ticked rows all have in the tag
// menu, and lets Shift-click tick a range. Loaded on the list
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
    // With everything ticked, the button lights up and selects none.
    var every = n > 0 && n === list.length;
    all.classList.toggle("on", every);
    all.title = all.getAttribute(every ? "data-none" : "data-all");
    all.setAttribute("aria-label", all.title);
    count.textContent = n ? plural(count, n) : "";
    bar.classList.toggle("has-selection", n > 0);
    bar.querySelectorAll(".bulk-actions > button").forEach(function (b) {
      b.disabled = b.getAttribute("data-needs") === "one" ? n !== 1 : n === 0;
    });
    if (title) title.textContent = plural(title, n);
  }

  // Opening the tag menu presses the tags that every ticked row has, as
  // picking one of those takes it off them, and half-presses ("mixed") the
  // ones only some have.
  var tagMenu = document.getElementById("bulk-tag");
  if (tagMenu) tagMenu.addEventListener("beforetoggle", function (e) {
    if (e.newState !== "open") return;
    var common = null, any = [];
    boxes().filter(function (b) { return b.checked; }).forEach(function (box) {
      var names = Array.prototype.map.call(box.closest("li.bookmark").querySelectorAll(".bm-tags [data-tag-name]"), function (a) { return a.getAttribute("data-tag-name"); });
      common = common ? common.filter(function (x) { return names.indexOf(x) >= 0; }) : names;
      any = any.concat(names);
    });
    tagMenu.querySelectorAll("[data-tag-name]").forEach(function (b) {
      var name = b.getAttribute("data-tag-name");
      b.setAttribute("aria-pressed", common && common.indexOf(name) >= 0 ? "true" : any.indexOf(name) >= 0 ? "mixed" : "false");
    });
  });

  all.setAttribute("data-all", all.title);
  all.hidden = false;

  all.addEventListener("click", function () {
    var on = !all.classList.contains("on"); // with some ticked, it ticks them all
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
