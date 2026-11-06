// Searching, changing view or tag, paging and the toolbar's actions on the
// list swap the content of <main> in place of a page load. Links and forms
// marked data-swap are ordinary ones, so without this script they just load
// the page, and so they do if a swap fails.
//
// Back and forward show the page as it was, as a browser's own history does,
// from what's kept here. Anything that changes bookmarks empties it.
(function () {
  "use strict";

  // It closes popovers, and an older browser can't even ask about them:
  // there, the links and forms just load the page.
  if (!HTMLElement.prototype.showPopover) return;

  var main = document.querySelector("main");
  var input = document.getElementById("q"); // stays through every swap
  var pending = null; // the page being fetched: a newer one cancels it
  var saving = null; // the action being posted: it's never cancelled
  var typed = ""; // the search box when the request began
  var top = 0; // how far down the page was before the last swap
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
    var typing = document.activeElement === input && input.value !== typed;
    if (!typing) input.value = fresh.querySelector("#q").value;
    fresh.replaceWith(row);
    main.replaceChildren(next.content);
    settled();
    var heading = main.querySelector(".list-head h1");
    document.title = heading.textContent + " · Anchor";
    // What had focus is gone, or would keep a phone's keyboard open. Someone
    // typing a new search while this was on its way keeps going.
    heading.tabIndex = -1;
    if (typing) input.focus();
    else heading.focus({ preventScroll: true });
    document.dispatchEvent(new CustomEvent("anchor:content-swapped"));
  }

  // start cancels what's under way and answers at once: menus close, and
  // the list dims if the response doesn't come straight away.
  function start() {
    if (pending) pending.abort();
    typed = input.value;
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
        top = window.scrollY;
        swap(html);
        return res.url;
      });
    });
  }

  function go(url, push, scroll) {
    // After an action, or this could read the list from before it.
    if (saving) {
      saving.then(function () { go(url, push, scroll); });
      return;
    }
    request(url, { signal: start() }).then(function (end) {
      // Like a browser, the same page again adds nothing to the history.
      history[push && end !== location.href ? "pushState" : "replaceState"](null, "", end);
      here = location.href;
      window.scrollTo(0, scroll);
    }).catch(function (err) {
      if (err.name !== "AbortError") location.href = url;
    });
  }

  function rows() {
    return Array.prototype.slice.call(main.querySelectorAll("li.bookmark"));
  }

  // send posts a form as the browser would. The list it redirects to takes
  // this history entry's place, and the page and the keyboard's place in the
  // rows stay where they are.
  function send(form, button) {
    var body = new URLSearchParams(new FormData(form));
    if (button && button.name) body.append(button.name, button.value);
    var url = (button && button.getAttribute("formaction")) || form.getAttribute("action");
    var active = document.activeElement;
    var place = rows().indexOf(active && active.closest("li.bookmark"));
    saved.clear();
    start();
    saving = request(url, { method: "POST", body: body }).then(function (end) {
      history.replaceState(null, "", end);
      here = location.href;
      var row = rows()[Math.min(place, rows().length - 1)];
      if (place >= 0 && row && document.activeElement !== input) row.querySelector(".bm-title").focus({ preventScroll: true });
      window.scrollTo(0, top); // the browser anchors the replaced page elsewhere
    }).catch(function (err) {
      var res = err.response;
      if (res && res.ok) {
        location.href = res.url;
      } else if (res && !res.redirected && res.status < 500) { // refused, so posting it again shows why
        native = true;
        form.requestSubmit(button);
      } else {
        location.reload(); // it may have gone through, and a tag would toggle back: show what the server has
      }
    }).then(function () { saving = null; });
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
      if (!saving) send(form, e.submitter); // a double click isn't a second action
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
    typed = input.value;
    swap(html);
    window.scrollTo(0, scroll);
  });
})();
