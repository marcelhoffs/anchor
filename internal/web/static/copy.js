// Copy buttons for values shown only once (invite links, passwords, API
// tokens, recovery codes).
// Markup contract:
//   <button type="button" data-copy="<target id>" data-copied="<feedback>" hidden>
//   <span role="status" data-copy-status></span>  (inside the button's parent)
// An input target copies its value; any other copies its items' text, one
// per line. The clipboard needs a secure context (HTTPS or localhost), so
// without it, or without this script, the buttons stay hidden and the value
// can still be selected by hand.
(function () {
  "use strict";

  if (!navigator.clipboard) return;

  document.querySelectorAll("button[data-copy]").forEach(function (btn) {
    var target = document.getElementById(btn.dataset.copy);
    if (!target) return;
    var status = btn.parentElement.querySelector("[data-copy-status]");
    var timer = 0;
    btn.hidden = false;

    btn.addEventListener("click", function () {
      var text = target.matches("input, textarea")
        ? target.value
        : Array.prototype.map.call(target.querySelectorAll("li"), function (li) {
            return li.textContent.trim();
          }).join("\n");
      navigator.clipboard.writeText(text).then(function () {
        if (!status) return;
        status.textContent = btn.dataset.copied;
        clearTimeout(timer);
        timer = setTimeout(function () { status.textContent = ""; }, 3000);
      }).catch(function () {
        if (target.select) target.select();
      });
    });
  });
})();
