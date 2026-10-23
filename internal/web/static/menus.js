// Top bar menus (<details class="top-menu">): opening one closes the others,
// and a click outside or Escape closes them. Without this script each menu
// still opens and closes from its own button.
(function () {
  "use strict";

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
