// The search box's choice between the bookmarks and the web (list.html):
// the placeholder says what is searched, and a web search opens the engine
// in a new tab. Without this script the list page sends it on instead.
// The last choice is remembered in this browser.
(function () {
  "use strict";

  var form = document.querySelector("form.search");
  var input = form && form.querySelector("input[name=q]");
  if (!input) return;
  var chosen = function () { return form.querySelector("input[name=in]:checked"); };
  var key = "anchor-search-in";

  form.addEventListener("change", function (e) {
    if (e.target.name !== "in") return;
    input.placeholder = e.target.dataset.placeholder;
    input.focus();
    try { localStorage.setItem(key, e.target.value); } catch (err) {}
  });

  // Start in the last choice: as picker.js does, the button shows its icon.
  var last = null;
  try { last = localStorage.getItem(key); } catch (err) {}
  form.querySelectorAll("input[name=in]").forEach(function (option) {
    if (option.value !== last || option.checked) return;
    option.checked = true;
    input.placeholder = option.dataset.placeholder;
    var copy = option.closest("label").cloneNode(true);
    copy.querySelector("input").remove();
    form.querySelector(".search-in .picker-current").innerHTML = copy.innerHTML;
  });

  form.addEventListener("submit", function (e) {
    var option = chosen();
    if (option && option.dataset.url && input.value.trim()) {
      e.preventDefault();
      window.open(option.dataset.url + encodeURIComponent(input.value.trim()), "_blank", "noopener");
    } else if (option) {
      option.disabled = true; // keeps "in=" out of the bookmark search's URL
    }
  });
  // Back on the page from the browser's history, or with the results swapped
  // in by listnav.js, the option counts again.
  function restore() {
    var option = chosen();
    if (option) option.disabled = false;
  }
  addEventListener("pageshow", restore);
  document.addEventListener("anchor:content-swapped", restore);
})();
