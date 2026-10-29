// The search box's choice between the bookmarks and the web (list.html):
// the placeholder says what is searched, and a web search goes straight to
// the engine. Without this script the list page sends it on instead.
(function () {
  "use strict";

  var form = document.querySelector("form.search");
  var input = form && form.querySelector("input[name=q]");
  if (!input) return;
  var chosen = function () { return form.querySelector("input[name=in]:checked"); };

  form.addEventListener("change", function (e) {
    if (e.target.name !== "in") return;
    input.placeholder = e.target.dataset.placeholder;
    input.focus();
  });
  form.addEventListener("submit", function (e) {
    var option = chosen();
    if (option && option.dataset.url && input.value.trim()) {
      e.preventDefault();
      location.assign(option.dataset.url + encodeURIComponent(input.value.trim()));
    } else if (option) {
      option.disabled = true; // keeps "in=" out of the bookmark search's URL
    }
  });
  // Back on the page from the browser's history, the option counts again.
  addEventListener("pageshow", function () {
    var option = chosen();
    if (option) option.disabled = false;
  });
})();
