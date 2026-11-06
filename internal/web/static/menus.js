// Top bar menus (<details class="top-menu">): opening one closes the others,
// and a click outside or Escape closes them. Without this script each menu
// still opens and closes from its own button.
//
// Also: shows notices and errors (data-toast) as toasts on top of the page,
// and fades them out after 5 seconds, waiting while the pointer or focus is
// on one, as the archive notice has an Undo. Each page or swap replaces the
// last ones. Without this script they stay where the page has them.
// Registers the service worker; and iOS shows :active (the pressed look, see style.css) on touch only
// when something listens for touchstart.
(function () {
  "use strict";

  document.addEventListener("touchstart", function () {}, { passive: true });

  var toasts = document.querySelector(".toasts");

  function fade(n) {
    var timer;
    function start() {
      clearTimeout(timer);
      timer = setTimeout(function () {
        n.classList.add("fading");
        // Without a transition (reduced motion) there's no transitionend.
        var gone = parseFloat(getComputedStyle(n).transitionDuration) || 0;
        timer = setTimeout(function () {
          n.remove();
          if (!toasts.children.length && toasts.matches(":popover-open")) toasts.hidePopover();
        }, gone * 1000);
      }, 5000);
    }
    function hold() { clearTimeout(timer); n.classList.remove("fading"); }
    n.addEventListener("mouseenter", hold);
    n.addEventListener("focusin", hold);
    n.addEventListener("mouseleave", function () { if (!n.contains(document.activeElement)) start(); });
    n.addEventListener("focusout", function (e) { if (!n.contains(e.relatedTarget) && !n.matches(":hover")) start(); });
    start();
  }

  function showToasts() {
    var fresh = document.querySelectorAll("main [data-toast]");
    toasts.replaceChildren.apply(toasts, fresh);
    fresh.forEach(fade);
    // Shown again, so they're on top of a menu opened since.
    if (toasts.matches(":popover-open")) toasts.hidePopover();
    if (fresh.length) toasts.showPopover();
  }
  document.addEventListener("anchor:content-swapped", showToasts); // listnav.js, settings.js

  // Installed as an app, the service worker (sw.js) shows an offline page
  // instead of the browser's error.
  if ("serviceWorker" in navigator) {
    navigator.serviceWorker.register("/sw.js").catch(function () {});
  }

  var menus = document.querySelectorAll(".top-menu");

  function closeAll(except) {
    menus.forEach(function (m) {
      if (m !== except) m.open = false;
    });
  }

  menus.forEach(function (menu) {
    menu.addEventListener("toggle", function () {
      if (menu.open) closeAll(menu);
    });
  });

  document.addEventListener("click", function (e) {
    if (!e.target.closest(".top-menu")) closeAll();
  });

  document.addEventListener("keydown", function (e) {
    if (e.key !== "Escape") return;
    var open = document.querySelector(".top-menu[open]");
    if (!open) return;
    open.open = false;
    open.querySelector("summary").focus();
  });

  showToasts();
})();
