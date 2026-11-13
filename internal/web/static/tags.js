// The tags field as coloured chips, with a type-ahead of the user's existing
// tags. Tags are separated by commas and may contain spaces.
// Markup contract:
//   <input name="…" role="combobox" aria-controls="<list id>"
//     data-tags="tag1,tag two,…" data-colors="blue,teal,…" data-remove="Remove tag %s">
//   <ul id="<list id>" role="listbox" hidden></ul>
// data-tags lists the tags most used first, data-colors their colours in the
// same order. Only those tags can be chosen: typed text that isn't one is
// dropped. The script takes the field's name for a hidden input that holds
// the chips as "a, b"; the field itself is left for finding the next tag.
// Without this script it is a plain text box.
(function () {
  "use strict";

  var MAX = 8;

  function norm(t) {
    return t.trim().replace(/^#/, "").replace(/\s+/g, " ").toLowerCase();
  }

  function chip(name, color) {
    var span = document.createElement("span");
    span.className = "tag";
    if (color) span.dataset.color = color;
    span.appendChild(document.createTextNode(name));
    return span;
  }

  function setup(input) {
    var names = input.dataset.tags.split(",").filter(Boolean);
    var colors = (input.dataset.colors || "").split(",");
    var list = document.getElementById(input.getAttribute("aria-controls"));
    if (!list) return;
    var known = {}; // lower-cased name → {name, color}
    names.forEach(function (n, i) { known[n.toLowerCase()] = { name: n, color: colors[i] }; });
    var shown = [], active = -1;
    // The list shows the tags alphabetically.
    var order = names.map(function (n, i) { return i; }).sort(function (a, b) {
      return names[a].localeCompare(names[b], undefined, { sensitivity: "base" });
    });

    // The box the chips and the field sit in; the hidden input submits them.
    var box = document.createElement("div");
    box.className = "tag-input";
    input.parentNode.insertBefore(box, input);
    var chips = document.createElement("span");
    chips.className = "tag-chips";
    box.appendChild(chips);
    box.appendChild(input);
    var value = document.createElement("input");
    value.type = "hidden";
    value.name = input.name;
    input.removeAttribute("name");
    box.parentNode.insertBefore(value, box.nextSibling);
    var placeholder = input.placeholder;

    function tags() {
      return Array.prototype.map.call(chips.children, function (c) { return c.dataset.name; });
    }

    function sync() {
      value.value = tags().join(", ");
      input.placeholder = chips.children.length ? "" : placeholder;
    }

    // add makes a chip of name, spelled and coloured as the existing tag. It
    // reports whether name is one; an unknown name gets no chip.
    function add(name) {
      var key = norm(name), tag = known[key];
      if (!tag) return false;
      if (tags().some(function (t) { return norm(t) === key; })) return true;
      var c = chip(tag.name, tag.color);
      c.dataset.name = tag.name;
      var remove = document.createElement("button");
      remove.type = "button";
      remove.className = "tag-remove";
      remove.setAttribute("aria-label", (input.dataset.remove || "%s").replace("%s", tag.name));
      remove.textContent = "×";
      remove.addEventListener("click", function () {
        c.remove();
        sync();
        input.focus();
      });
      c.appendChild(remove);
      chips.appendChild(c);
      return true;
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
      // With nothing typed it lists every tag, but only while the field has
      // no chips yet.
      var text = norm(input.value);
      if (!text && chips.children.length) return close();
      var used = {};
      tags().forEach(function (t) { used[norm(t)] = true; });
      var starts = [], contains = [];
      order.forEach(function (i) {
        var ln = names[i].toLowerCase();
        if (used[ln]) return;
        if (ln.indexOf(text) === 0) starts.push(i);
        else if (ln.indexOf(text) > 0) contains.push(i);
      });
      shown = starts.concat(contains);
      if (text) shown = shown.slice(0, MAX);
      if (!shown.length) return close();
      list.textContent = "";
      shown.forEach(function (n, i) {
        var li = document.createElement("li");
        li.id = list.id + "-" + i;
        li.setAttribute("role", "option");
        li.setAttribute("aria-selected", "false");
        li.appendChild(chip(names[n], colors[n]));
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
      add(names[shown[i]]);
      input.value = "";
      sync();
      close();
    }

    // commit makes a chip of what's typed if it names a tag, and reports
    // whether anything was typed. Unknown text stays to be corrected, unless
    // drop: then it goes.
    function commit(drop) {
      if (!norm(input.value)) return false;
      if (add(input.value) || drop) input.value = "";
      sync();
      close();
      return true;
    }

    // The field starts with the saved tags as text: they become chips.
    input.value.split(",").forEach(add);
    input.value = "";
    sync();

    box.addEventListener("mousedown", function (e) {
      if (e.target === box || e.target === chips) {
        e.preventDefault();
        input.focus();
      }
    });
    // Focusing or clicking an empty field opens the list.
    input.addEventListener("focus", update);
    input.addEventListener("click", function () {
      if (list.hidden) update();
    });
    input.addEventListener("input", function () {
      // A comma, typed or pasted, ends a tag; unknown names are dropped.
      var parts = input.value.split(",");
      if (parts.length > 1) {
        input.value = parts.pop().replace(/^\s+/, "");
        parts.forEach(add);
      }
      sync();
      update();
    });
    input.addEventListener("blur", function () {
      commit(true);
    });
    input.addEventListener("keydown", function (e) {
      if (e.key === "Backspace" && input.value === "" && chips.lastChild) {
        chips.lastChild.remove();
        sync();
        return;
      }
      if (list.hidden) {
        if (e.key === "ArrowDown") update();
        // Enter makes a chip of a typed tag name; with nothing typed it submits.
        if (e.key === "Enter" && commit()) e.preventDefault();
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
          // The highlighted suggestion, or else what's typed. With neither
          // the list closes and the form submits.
          if (active < 0 && !norm(input.value)) { close(); break; }
          e.preventDefault();
          if (active >= 0) accept(active);
          else commit();
          break;
        case "Tab":
          // Tab takes the highlighted suggestion, or else the first. With
          // neither, it moves on as usual.
          if (e.shiftKey) break;
          if (active < 0 && !norm(input.value)) { close(); break; }
          e.preventDefault();
          accept(active >= 0 ? active : 0);
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
