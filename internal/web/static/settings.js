// The settings forms marked data-save. Appearance choices show at once, as
// a preview until they're saved. Saving keeps your place on the page: the
// server's answer, a notice or an error, shows as a toast (menus.js).
// Changing the language loads the page, as all of its text changes.
// Without this script the form posts and the page reloads with the notice
// on top, and so it does if saving this way fails.
//
// shortcut: the browser's theme colour and the tab icon stay as they were
// until the next page load, and setting them here is more code than a page
// load's wait is worth.
(function () {
  "use strict";

  var root = document.documentElement;

  function language(form) {
    return form.elements.language && form.elements.language.value;
  }

  // The fields are named after the attributes on <html> they change.
  document.addEventListener("change", function (e) {
    if (e.target.closest("form[data-save]") && e.target.name in root.dataset) root.dataset[e.target.name] = e.target.value;
  });

  if (!HTMLElement.prototype.showPopover) return; // no toasts to show it in

  document.querySelectorAll("form[data-save]").forEach(function (form) {
    var savedLanguage = language(form);

    form.addEventListener("submit", function (e) {
      e.preventDefault();
      fetch(form.getAttribute("action"), { method: "POST", body: new URLSearchParams(new FormData(form)) }).then(function (res) {
        // Saved, or refused with a reason; anything else, such as the login page, isn't ours.
        if (res.status !== 422 && !/[?&]done=/.test(res.url)) throw new Error(res.status);
        return res.text().then(function (html) { return { html: html, url: res.url }; });
      }).then(function (page) {
        var message = new DOMParser().parseFromString(page.html, "text/html").querySelector("main [data-toast]");
        if (!message) throw new Error("no message");
        if (message.matches(".notice") && language(form) !== savedLanguage) {
          location.href = page.url; // it says so in the new language
          return;
        }
        document.querySelector("main").append(message);
        document.dispatchEvent(new CustomEvent("anchor:content-swapped")); // menus.js shows it
      }).catch(function () {
        form.submit(); // saving twice does no harm
      });
    });
  });
})();
