// Top bar menus (<details class="top-menu">): opening one closes the others,
// and a click outside or Escape closes them. Without this script each menu
// still opens and closes from its own button.
//
// Also: registers the service worker; and iOS shows :active (the pressed look, see style.css) on touch only
// when something listens for touchstart.
(function () {
  "use strict";

  document.addEventListener("touchstart", function () {}, { passive: true });

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
