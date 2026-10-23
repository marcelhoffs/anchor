// Type-ahead for the add-member field: suggests usernames from the server as
// the space's owner types.
// Markup contract:
//   <input role="combobox" aria-controls="<list id>" data-suggest="<url>">
//   <ul id="<list id>" role="listbox" hidden></ul>
// The URL gets ?q=<typed text> and answers a JSON array of usernames.
// Without this script the field is a plain text box.
(function () {
  "use strict";

  function setup(input) {
    var list = document.getElementById(input.getAttribute("aria-controls"));
    if (!list) return;
    var shown = [], active = -1, timer = 0, asked = 0;

    function close() {
      list.hidden = true;
      list.textContent = "";
      shown = [];
      active = -1;
      input.setAttribute("aria-expanded", "false");
      input.removeAttribute("aria-activedescendant");
    }

    function highlight(i) {
      active = i;
      Array.prototype.forEach.call(list.children, function (li, j) {
        li.setAttribute("aria-selected", j === i ? "true" : "false");
      });
      if (i >= 0) {
        input.setAttribute("aria-activedescendant", list.children[i].id);
        list.children[i].scrollIntoView({ block: "nearest" });
      } else {
        input.removeAttribute("aria-activedescendant");
      }
    }

    function show(names) {
      shown = names;
      if (!shown.length) return close();
      list.textContent = "";
      shown.forEach(function (name, i) {
        var li = document.createElement("li");
        li.id = list.id + "-" + i;
        li.setAttribute("role", "option");
        li.setAttribute("aria-selected", "false");
        li.textContent = name;
        // mousedown, not click: keep focus in the input.
        li.addEventListener("mousedown", function (e) {
          e.preventDefault();
          accept(i);
        });
        list.appendChild(li);
      });
      list.hidden = false;
      input.setAttribute("aria-expanded", "true");
      highlight(-1);
    }

    function accept(i) {
      input.value = shown[i];
      close();
    }

    // ask fetches suggestions for what's typed; an answer that arrives after
    // a newer question, or after the field lost focus, is dropped.
    function ask() {
      var q = input.value.trim(), n = ++asked;
      if (!q) return close();
      fetch(input.dataset.suggest + "?q=" + encodeURIComponent(q), { headers: { Accept: "application/json" } })
        .then(function (r) { return r.ok ? r.json() : []; })
        .then(function (names) {
          if (n === asked && document.activeElement === input) show(names);
        })
        .catch(close);
    }

    input.addEventListener("input", function () {
      clearTimeout(timer);
      timer = setTimeout(ask, 150);
    });
    input.addEventListener("blur", function () {
      asked++;
      close();
    });
    input.addEventListener("keydown", function (e) {
      if (list.hidden) {
        if (e.key === "ArrowDown") ask();
        return;
      }
      switch (e.key) {
        case "ArrowDown":
          e.preventDefault();
          highlight((active + 1) % shown.length);
          break;
        case "ArrowUp":
          e.preventDefault();
          highlight(active <= 0 ? shown.length - 1 : active - 1);
          break;
        case "Enter":
        case "Tab":
          // Tab takes the first suggestion; Enter only a highlighted one, so
          // Enter on its own still submits the form.
          if (active >= 0 || (e.key === "Tab" && !e.shiftKey)) {
            e.preventDefault();
            accept(active >= 0 ? active : 0);
          }
          break;
        case "Escape":
          e.preventDefault();
          close();
          break;
      }
    });
  }

  document.querySelectorAll("input[role=combobox][data-suggest]").forEach(setup);
})();
