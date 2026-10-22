// Passkey (WebAuthn) registration and sign-in. Loaded only on pages that
// offer passkeys. Markup contract:
//   <div data-passkey-scope>
//     <button data-passkey="register|login" data-begin="/…" data-finish="/…">
//     <input data-passkey-name>            (register only, optional)
//     <p data-passkey-status hidden></p>  (error messages)
//   </div>
(function () {
  "use strict";

  var csrf = (document.querySelector('meta[name="csrf-token"]') || {}).content || "";

  function decode(s) {
    s = s.replace(/-/g, "+").replace(/_/g, "/");
    while (s.length % 4) s += "=";
    var bin = atob(s), out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out.buffer;
  }

  function encode(buf) {
    var bytes = new Uint8Array(buf), bin = "";
    for (var i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
    return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }

  function show(scope, msg) {
    var box = scope.querySelector("[data-passkey-status]");
    if (box) {
      box.textContent = msg;
      box.hidden = !msg;
    }
  }

  function post(url, body) {
    return fetch(url, {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf },
      body: JSON.stringify(body || {})
    }).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (data) {
        if (data.redirect) {
          window.location.href = data.redirect;
          return new Promise(function () {}); // navigating away
        }
        if (!res.ok) throw new Error(data.error || "Something went wrong. Try again.");
        return data;
      });
    });
  }

  function register(button, scope) {
    var nameInput = scope.querySelector("[data-passkey-name]");
    var name = nameInput ? nameInput.value.trim() : "";
    return post(button.dataset.begin).then(function (opts) {
      var pk = opts.publicKey;
      pk.challenge = decode(pk.challenge);
      pk.user.id = decode(pk.user.id);
      (pk.excludeCredentials || []).forEach(function (c) { c.id = decode(c.id); });
      return navigator.credentials.create({ publicKey: pk });
    }).then(function (cred) {
      var r = cred.response;
      return post(button.dataset.finish + "?name=" + encodeURIComponent(name), {
        id: cred.id,
        rawId: encode(cred.rawId),
        type: cred.type,
        response: {
          attestationObject: encode(r.attestationObject),
          clientDataJSON: encode(r.clientDataJSON),
          transports: r.getTransports ? r.getTransports() : []
        },
        clientExtensionResults: cred.getClientExtensionResults()
      });
    });
  }

  function login(button) {
    return post(button.dataset.begin).then(function (opts) {
      var pk = opts.publicKey;
      pk.challenge = decode(pk.challenge);
      (pk.allowCredentials || []).forEach(function (c) { c.id = decode(c.id); });
      return navigator.credentials.get({ publicKey: pk });
    }).then(function (cred) {
      var r = cred.response;
      return post(button.dataset.finish, {
        id: cred.id,
        rawId: encode(cred.rawId),
        type: cred.type,
        response: {
          authenticatorData: encode(r.authenticatorData),
          clientDataJSON: encode(r.clientDataJSON),
          signature: encode(r.signature),
          userHandle: r.userHandle ? encode(r.userHandle) : null
        },
        clientExtensionResults: cred.getClientExtensionResults()
      });
    });
  }

  var supported = window.PublicKeyCredential && window.isSecureContext;
  document.querySelectorAll("[data-passkey]").forEach(function (button) {
    var scope = button.closest("[data-passkey-scope]") || document.body;
    if (!supported) {
      button.disabled = true;
      show(scope, window.isSecureContext
        ? "This browser doesn't support passkeys."
        : "Passkeys need a secure (HTTPS) connection to Burrow.");
      return;
    }
    button.addEventListener("click", function (ev) {
      ev.preventDefault();
      show(scope, "");
      button.disabled = true;
      var run = button.dataset.passkey === "register" ? register(button, scope) : login(button);
      run.catch(function (err) {
        button.disabled = false;
        // NotAllowedError: the user cancelled or the prompt timed out.
        show(scope, err && err.name === "NotAllowedError"
          ? "Passkey prompt was cancelled. Try again when you're ready."
          : (err && err.message) || "Something went wrong. Try again.");
      });
    });
  });
})();
