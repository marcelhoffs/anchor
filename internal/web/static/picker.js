// Dropdowns of radios (see .picker in style.css): choosing an option shows
// it on the button and closes the menu. Without this script the menu still
// works; the button shows the saved choice until the form is submitted.
(function () {
  "use strict";

  document.querySelectorAll(".picker").forEach(function (button) {
    var menu = document.getElementById(button.getAttribute("popovertarget"));
    var current = button.querySelector(".picker-current");
    if (!menu || !current) return;
    menu.addEventListener("change", function (e) {
      var label = e.target.closest("label");
      if (!label) return;
      var copy = label.cloneNode(true);
      copy.querySelector("input").remove();
      current.innerHTML = copy.innerHTML;
      menu.hidePopover();
      button.focus();
    });
  });
})();
