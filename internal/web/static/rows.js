// The list page has one delete confirmation for all its rows. A row's
// button that opens it fills it in first: the bookmark's
// title in the text, and its data-action as where the confirm button posts.
// Loaded on the list page only when the space can be edited. Without this
// script the confirm button stays disabled.
(function () {
  "use strict";

  document.addEventListener("click", function (e) {
    var opener = e.target.closest("button[data-action][popovertarget]");
    if (!opener) return;
    var dialog = document.getElementById(opener.getAttribute("popovertarget"));
    var row = opener.closest("li.bookmark");
    if (!dialog || !row) return;
    dialog.querySelector(".confirm-text strong").textContent = row.querySelector(".bm-title").textContent;
    var ok = dialog.querySelector("button[form]");
    ok.formAction = opener.dataset.action;
    ok.disabled = false;
  });
})();
