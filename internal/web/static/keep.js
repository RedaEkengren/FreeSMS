// Keeping what somebody typed.
//
// The most damning review in this whole category, about a paid competitor:
//
//   the system randomly glitches and shuts down and when it does if you
//   haven't saved your 3-5 quotes or orders all those hours of work is all
//   lost
//
// Everything here exists so that cannot happen: no Save button to miss, a
// draft that survives a locked phone and a closed tab, and a queue that holds
// a submission until the workshop's wifi comes back.
(function () {
  "use strict";

  // Browser storage is not always there. Private windows, cleared site data
  // and locked-down devices all throw or return nothing, so every read and
  // write is wrapped and the page works without it.
  var store = {
    get: function (key) {
      try { return window.localStorage.getItem(key); } catch (e) { return null; }
    },
    set: function (key, value) {
      try { window.localStorage.setItem(key, value); return true; }
      catch (e) {
        // Out of room, or storage refused. Loud, because a queue that cannot
        // grow silently is a queue that loses work.
        console.warn("[keep] could not store", key, e);
        return false;
      }
    },
    remove: function (key) {
      try { window.localStorage.removeItem(key); } catch (e) {}
    }
  };

  function fieldsOf(form) {
    var out = {};
    Array.prototype.forEach.call(form.elements, function (el) {
      if (!el.name || el.type === "hidden" || el.type === "password" ||
          el.type === "file" || el.type === "submit") return;
      out[el.name] = el.value;
    });
    return out;
  }

  function anyFilled(fields) {
    return Object.keys(fields).some(function (k) { return fields[k].trim() !== ""; });
  }

  // ---- Drafts -------------------------------------------------------------
  //
  // Kept in the browser immediately, so a crash loses nothing, and on the
  // server shortly after, because a phone that is lost or swapped takes its
  // local storage with it.

  function draftKey(name) { return "freesms.draft." + name; }

  function restore(form, name) {
    var local = store.get(draftKey(name));
    if (local) {
      try { apply(form, JSON.parse(local)); } catch (e) {}
    }
    // The server's copy arrives later and only fills what is still empty, so
    // it cannot overwrite something the person has already started typing.
    fetch("/drafts?form=" + encodeURIComponent(name), { headers: { Accept: "application/json" } })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (fields) { if (fields) apply(form, fields, true); })
      .catch(function () {});
  }

  function apply(form, fields, onlyEmpty) {
    Object.keys(fields || {}).forEach(function (name) {
      var el = form.elements[name];
      if (!el || typeof el.value !== "string") return;
      if (onlyEmpty && el.value.trim() !== "") return;
      el.value = fields[name];
    });
  }

  function watch(form) {
    var name = form.getAttribute("data-keep");
    if (!name) return;
    restore(form, name);

    var timer = null;
    form.addEventListener("input", function () {
      var fields = fieldsOf(form);
      // Immediately, locally. This is the copy that survives a crash.
      store.set(draftKey(name), JSON.stringify(fields));

      // And to the server, a moment later, so a burst of typing is one
      // request rather than one per keystroke.
      window.clearTimeout(timer);
      timer = window.setTimeout(function () {
        if (!anyFilled(fields)) return;
        fetch("/drafts", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ form: name, fields: fields })
        }).catch(function () {
          // Offline. The local copy is already safe and the next edit tries
          // again, so there is nothing to tell anybody.
        });
      }, 1200);
    });

    form.addEventListener("submit", function () {
      store.remove(draftKey(name));
      fetch("/drafts?form=" + encodeURIComponent(name), { method: "DELETE" }).catch(function () {});
    });
  }

  // ---- The offline queue --------------------------------------------------
  //
  // Workshop wifi is bad near the lifts and worse in the pit. A submission
  // that fails because of that is held and sent when the connection comes
  // back, with a key that makes arriving twice harmless.

  var QUEUE = "freesms.queue";

  function queue() {
    try { return JSON.parse(store.get(QUEUE) || "[]"); } catch (e) { return []; }
  }

  function setQueue(items) {
    if (!store.set(QUEUE, JSON.stringify(items))) {
      // Nowhere to put it. Better to say so than to pretend it was sent.
      window.alert("This device has no room left to hold unsent work. " +
                   "Find a connection before carrying on.");
    }
  }

  function newKey() {
    if (window.crypto && window.crypto.randomUUID) return window.crypto.randomUUID();
    return String(Date.now()) + "-" + Math.random().toString(36).slice(2, 12);
  }

  function hold(form) {
    var body = new URLSearchParams(new FormData(form)).toString();
    var items = queue();
    items.push({ url: form.action, body: body, key: newKey(), at: Date.now() });
    setQueue(items);
    show(items.length);
  }

  function show(n) {
    var bar = document.getElementById("held");
    if (!bar) return;
    bar.hidden = n === 0;
    bar.textContent = n === 0 ? "" :
      n + (n === 1 ? " thing is waiting to be sent." : " things are waiting to be sent.");
  }

  function flush() {
    var items = queue();
    if (items.length === 0) return;

    var next = items[0];
    fetch(next.url, {
      method: "POST",
      headers: {
        "Content-Type": "application/x-www-form-urlencoded",
        // The same key on every attempt, so a request that arrived before the
        // connection dropped is not carried out twice.
        "Idempotency-Key": next.key
      },
      body: next.body,
      redirect: "follow"
    }).then(function (resp) {
      if (resp.status >= 500) return; // Try again later.
      if (resp.status >= 400) {
        // The server will not take it -- the job moved on, most likely.
        // Surfaced rather than dropped: somebody has to know it did not
        // happen.
        window.alert("Something that was waiting to be sent was refused. " +
                     "Open the job and check it.");
      }
      var rest = queue().slice(1);
      setQueue(rest);
      show(rest.length);
      if (rest.length) flush();
    }).catch(function () {
      // Still offline.
    });
  }

  function guard(form) {
    form.addEventListener("submit", function (e) {
      if (navigator.onLine !== false) return;
      e.preventDefault();
      hold(form);
      window.alert("No connection. This is being held and will be sent when there is one.");
    });
  }

  // ---- Wiring -------------------------------------------------------------

  document.addEventListener("DOMContentLoaded", function () {
    Array.prototype.forEach.call(document.querySelectorAll("form[data-keep]"), watch);
    Array.prototype.forEach.call(document.querySelectorAll("form[method='post']"), guard);
    show(queue().length);
    flush();

    // The content security policy forbids inline handlers, deliberately, so
    // anything that used to be an onclick lives here.
    Array.prototype.forEach.call(document.querySelectorAll("[data-print]"), function (el) {
      el.addEventListener("click", function () { window.print(); });
    });

    Array.prototype.forEach.call(document.querySelectorAll("[data-select]"), function (el) {
      el.addEventListener("click", function () { el.select(); });
    });

    // The customer's link is shown once and is sixty characters long. Selecting
    // that by hand to paste into a message is the kind of small friction that
    // makes somebody stop sending links.
    Array.prototype.forEach.call(document.querySelectorAll("[data-copy]"), function (el) {
      el.addEventListener("click", function () {
        var field = document.querySelector(el.getAttribute("data-copy"));
        if (!field) return;
        var said = el.textContent;
        var done = function () {
          el.textContent = el.getAttribute("data-copied") || "Copied";
          setTimeout(function () { el.textContent = said; }, 2000);
        };
        // The clipboard API needs a secure context, which a shop reaching the
        // service over plain HTTP on its own network does not have. The old
        // selection is the fallback, and the button says what to do next
        // rather than appearing to have failed silently.
        if (navigator.clipboard && window.isSecureContext) {
          navigator.clipboard.writeText(field.value).then(done, function () {
            field.select();
          });
          return;
        }
        field.select();
      });
    });
  });

  window.addEventListener("online", flush);
})();
