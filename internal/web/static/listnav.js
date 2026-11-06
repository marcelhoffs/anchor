// Searching, changing view or tag, paging and the toolbar's actions on the
// list swap the content of <main> in place of a page load. Links and forms
// marked data-swap are ordinary ones, so without this script they just load
// the page, and so they do if a swap fails.
//
// Back and forward show the page as it was, as a browser's own history does,
// from what's kept here. Anything that changes bookmarks empties it.
(function () {
  "use strict";

  var main = document.querySelector("main");
  var pending = null; // the request under way: a newer one cancels it
  var timer = null;
  var native = false; // the next submit is the browser's own
  var here = location.href;
  var saved = new Map(); // page URL: its content, for back and forward
  var scrolls = {}; // page URL: how far down it was
  saved.set(here, main.innerHTML);

  addEventListener("scroll", function () { scrolls[here] = window.scrollY; }, { passive: true });
  document.addEventListener("anchor:list-changed", function () { saved.clear(); });

  function swap(html) {
    var next = document.createElement("template");
    next.innerHTML = html;
    // The search box stays, as search.js and picker.js are bound to it, and
    // takes the new page's text.
    var row = main.querySelector(".search-row");
    var fresh = next.content.querySelector(".search-row");
    row.querySelector("#q").value = fresh.querySelector("#q").value;
    fresh.replaceWith(row);
    main.replaceChildren(next.content);
    settled();
    var heading = main.querySelector(".list-head h1");
    document.title = heading.textContent + " · Anchor";
    // What had focus is gone, or would keep a phone's keyboard open.
    heading.tabIndex = -1;
    heading.focus({ preventScroll: true });
    document.dispatchEvent(new CustomEvent("anchor:content-swapped"));
  }

  // start cancels what's under way and answers at once: menus close, and
  // the list dims if the response doesn't come straight away.
  function start() {
    if (pending) pending.abort();
    clearTimeout(timer);
    timer = setTimeout(function () { main.setAttribute("aria-busy", "true"); }, 150);
    // One may have closed with the menu it opened from, hence the second look.
    main.querySelectorAll("[popover]:popover-open").forEach(function (p) {
      if (p.matches(":popover-open")) p.hidePopover();
    });
    pending = new AbortController();
    return pending.signal;
  }

  function settled() {
    clearTimeout(timer);
    main.removeAttribute("aria-busy");
  }

  // request gets the list's content and the URL it ended up at, which may be
  // another page of the list. Anything else, such as the login page, is an
  // error with the response on it.
  function request(url, options) {
    options.headers = { "X-Anchor-Nav": "1" };
    return fetch(url, options).then(function (res) {
      if (!res.ok || new URL(res.url).pathname !== "/") throw Object.assign(new Error(res.status), { response: res });
      return res.text().then(function (html) {
        saved.delete(res.url); // so it counts as the newest
        saved.set(res.url, html);
        if (saved.size > 20) saved.delete(saved.keys().next().value);
        swap(html);
        return res.url;
      });
    });
  }

  function go(url, push, scroll) {
    request(url, { signal: start() }).then(function (end) {
      if (push) history.pushState(null, "", end);
      else if (end !== url) history.replaceState(null, "", end);
      here = location.href;
      window.scrollTo(0, scroll);
    }).catch(function (err) {
      if (err.name !== "AbortError") location.href = url;
    });
  }

  // send posts a form as the browser would. The list it redirects to takes
  // this history entry's place, and the page stays where it is.
  function send(form, button) {
    var body = new URLSearchParams(new FormData(form));
    if (button && button.name) body.append(button.name, button.value);
    var url = (button && button.getAttribute("formaction")) || form.getAttribute("action");
    saved.clear();
    request(url, { method: "POST", body: body, signal: start() }).then(function (end) {
      history.replaceState(null, "", end);
      here = location.href;
    }).catch(function (err) {
      if (err.name === "AbortError") return;
      var res = err.response;
      if (!res) {
        location.reload(); // it may have gone through: show what the server has
      } else if (res.ok) {
        location.href = res.url;
      } else { // refused, so posting it again shows why
        native = true;
        form.requestSubmit(button);
      }
    });
  }

  // press marks the view or tag just clicked without waiting for its page.
  function press(link) {
    var href = link.getAttribute("href");
    var views = Array.prototype.slice.call(main.querySelectorAll(".view-bar a, .side-link"));
    if (!views.some(function (a) { return a.getAttribute("href") === href; })) return;
    views.forEach(function (a) {
      if (a.getAttribute("href") === href) a.setAttribute("aria-current", "page");
      else a.removeAttribute("aria-current");
    });
  }

  document.addEventListener("click", function (e) {
    var link = e.target.closest("a[data-swap]");
    if (!link || e.defaultPrevented || e.button !== 0 || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    press(link);
    go(link.href, true, 0);
  });

  // On the document, so this runs after search.js, which keeps web searches
  // to itself and takes "in" out of the form for a bookmark search.
  document.addEventListener("submit", function (e) {
    var form = e.target;
    if (native || e.defaultPrevented || !form.matches("form[data-swap]")) return;
    if (form.method !== "post") {
      e.preventDefault();
      go(new URL(form.getAttribute("action") + "?" + new URLSearchParams(new FormData(form)), location.href).href, true, 0);
    } else if (!e.submitter || e.submitter.value !== "edit") { // Edit leaves the list
      e.preventDefault();
      send(form, e.submitter);
    }
  });

  addEventListener("popstate", function () {
    if (location.href === here) return;
    var url = here = location.href;
    var scroll = scrolls[url] || 0;
    var html = saved.get(url);
    if (html === undefined) {
      go(url, false, scroll);
      return;
    }
    if (pending) pending.abort();
    swap(html);
    window.scrollTo(0, scroll);
  });
})();
