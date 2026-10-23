// Type-ahead for tag fields. Suggests the user's existing tags for the word
// being typed. Markup contract:
//   <input role="combobox" aria-controls="<list id>" data-tags="tag1 tag2 …">
//   <ul id="<list id>" role="listbox" hidden></ul>
// data-tags lists the tags most used first. Without this script the field
// is a plain text box.
(function () {
  "use strict";

  var MAX = 8;
  var SEP = /[\s,]/;

  function setup(input) {
    var all = input.dataset.tags.split(" ").filter(Boolean);
    var list = document.getElementById(input.getAttribute("aria-controls"));
    if (!all.length || !list) return;
    var shown = [], active = -1;

    // The word around the caret: [start, end) of input.value.
    function word() {
      var v = input.value, caret = input.selectionStart == null ? v.length : input.selectionStart;
      var start = caret, end = caret;
      while (start > 0 && !SEP.test(v[start - 1])) start--;
      while (end < v.length && !SEP.test(v[end])) end++;
      return { start: start, end: end, text: v.slice(start, end).replace(/^#/, "").toLowerCase() };
    }

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

    function update() {
      var w = word();
      if (!w.text) return close();
      var used = {};
      input.value.toLowerCase().split(SEP).forEach(function (t) { used[t.replace(/^#/, "")] = true; });
      var starts = [], contains = [];
      all.forEach(function (t) {
        var lt = t.toLowerCase();
        if (lt === w.text || (used[lt] && lt !== w.text)) return;
        if (lt.indexOf(w.text) === 0) starts.push(t);
        else if (lt.indexOf(w.text) > 0) contains.push(t);
      });
      shown = starts.concat(contains).slice(0, MAX);
      if (!shown.length) return close();
      list.textContent = "";
      shown.forEach(function (t, i) {
        var li = document.createElement("li");
        li.id = list.id + "-" + i;
        li.setAttribute("role", "option");
        li.setAttribute("aria-selected", "false");
        var hash = document.createElement("span");
        hash.className = "hash";
        hash.textContent = "#";
        li.appendChild(hash);
        li.appendChild(document.createTextNode(t));
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
      var w = word(), v = input.value, tag = shown[i];
      var after = v.slice(w.end).replace(/^[\s,]+/, "");
      var before = v.slice(0, w.start) + tag + " ";
      input.value = before + after;
      input.setSelectionRange(before.length, before.length);
      close();
    }

    input.addEventListener("input", update);
    input.addEventListener("blur", close);
    input.addEventListener("keydown", function (e) {
      if (list.hidden) {
        if (e.key === "ArrowDown" && word().text) update();
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

  document.querySelectorAll("input[role=combobox][data-tags]").forEach(setup);
})();
