// The settings forms marked data-save. Appearance choices show at once, as
// a preview until they're saved. Saving keeps your place on the page: the
// server's answer, a notice or an error, appears beside the Save button.
// Changing the language reloads the page, as all of its text changes.
// Without this script the form posts and the page reloads with the notice
// on top, and so it does if saving this way fails.
//
// shortcut: the browser's theme colour and the tab icon stay as they were
// until the next page load, and setting them here is more code than a page
// load's wait is worth.
(function () {
  "use strict";

  var root = document.documentElement;

  function language() {
    var chosen = document.querySelector("input[name=language]:checked");
    return chosen && chosen.value;
  }
  var savedLanguage = language();

  // The fields are named after the attributes on <html> they change.
  document.addEventListener("change", function (e) {
    if (e.target.closest("form[data-save]") && e.target.name in root.dataset) root.dataset[e.target.name] = e.target.value;
  });

  document.querySelectorAll("form[data-save]").forEach(function (form) {
    var status = document.createElement("div");
    status.className = "save-status";
    status.setAttribute("aria-live", "polite"); // there before its message, or it isn't read out
    (form.querySelector(".form-actions") || form).append(status); // beside the button

    form.addEventListener("submit", function (e) {
      e.preventDefault();
      fetch(form.getAttribute("action"), { method: "POST", body: new URLSearchParams(new FormData(form)) }).then(function (res) {
        // Saved, or refused with a reason; anything else, such as the login page, isn't ours.
        if (res.status !== 422 && !/[?&]done=/.test(res.url)) throw new Error(res.status);
        return res.text();
      }).then(function (html) {
        var message = new DOMParser().parseFromString(html, "text/html").querySelector("main .notice, main .error");
        if (!message) throw new Error("no message");
        if (message.matches(".notice") && language() !== savedLanguage) {
          location.reload();
          return;
        }
        status.replaceChildren(message);
        document.dispatchEvent(new CustomEvent("anchor:content-swapped")); // menus.js fades it
      }).catch(function () {
        form.submit(); // saving twice does no harm
      });
    });
  });
})();
