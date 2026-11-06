// Top bar menus (<details class="top-menu">): opening one closes the others,
// and a click outside or Escape closes them. Without this script each menu
// still opens and closes from its own button.
//
// Also: fades out notices (.notice[role=status]) after 5 seconds, waiting
// while the pointer or focus is on one, as the archive notice has an Undo;
// registers the service worker; and iOS shows :active (the pressed look, see style.css) on touch only
// when something listens for touchstart.
(function () {
  "use strict";

  document.addEventListener("touchstart", function () {}, { passive: true });

  function fade(n) {
    var timer;
    function start() {
      clearTimeout(timer);
      timer = setTimeout(function () {
        n.classList.add("fading");
        // Without a transition (reduced motion) there's no transitionend.
        var gone = parseFloat(getComputedStyle(n).transitionDuration) || 0;
        timer = setTimeout(function () { n.remove(); }, gone * 1000);
      }, 5000);
    }
    function hold() { clearTimeout(timer); n.classList.remove("fading"); }
    n.addEventListener("mouseenter", hold);
    n.addEventListener("focusin", hold);
    n.addEventListener("mouseleave", function () { if (!n.contains(document.activeElement)) start(); });
    n.addEventListener("focusout", function (e) { if (!n.contains(e.relatedTarget) && !n.matches(":hover")) start(); });
    start();
  }
  function fadeNotices() { document.querySelectorAll(".notice[role=status]").forEach(fade); }
  fadeNotices();
  document.addEventListener("anchor:content-swapped", fadeNotices); // listnav.js

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
})();
