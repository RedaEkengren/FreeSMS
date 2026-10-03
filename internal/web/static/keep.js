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

  // Keyed by the person as well as the form. They used to be keyed by the
  // form alone, so on a shared counter browser the next person to open the
  // intake form was handed the last person's half-typed customer. A user id
  // is a uuid and belongs to one shop, so it namespaces the shop as well.
  var DRAFT = "freesms.draft.";

  function draftKey(name) { return DRAFT + currentUser() + "." + name; }

  function restore(form, name) {
    var local = store.get(draftKey(name));
    if (local) {
      try { apply(form, JSON.parse(local)); } catch (e) {}
    }
    // The server's copy arrives later and only fills what is still as the page
    // arrived, so it cannot overwrite something the person has already typed.
    fetch("/drafts?form=" + encodeURIComponent(name), { headers: { Accept: "application/json" } })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (fields) { if (fields) apply(form, fields, true); })
      .catch(function () {});
  }

  function apply(form, fields, onlyUntouched) {
    Object.keys(fields || {}).forEach(function (name) {
      var el = form.elements[name];
      if (!el || typeof el.value !== "string") return;
      if (onlyUntouched && !untouched(el)) return;
      el.value = fields[name];
    });
  }

  // Whether a control still holds what the page arrived with. The server's
  // copy used to fill only empty fields, and a quantity that starts at 1, a
  // price at 0 and every select are never empty -- so a job line restored
  // from the server came back as its description alone.
  function untouched(el) {
    if (el.options) {
      for (var i = 0; i < el.options.length; i++) {
        if (el.options[i].defaultSelected) return el.selectedIndex === i;
      }
      return el.selectedIndex <= 0;
    }
    return el.value === el.defaultValue;
  }

  function watch(form) {
    var name = form.getAttribute("data-keep");
    // Nobody signed in -- sign-in, setup, the customer's page -- means no
    // draft is restored and none is kept. There is nobody to keep it for.
    if (!name || !currentUser()) return;
    restore(form, name);

    // The submission names its draft, so the server can say which one it
    // saved. Hidden, so it is never itself kept as part of the draft, and
    // inside the form, so a submission held offline carries it as well.
    var tag = document.createElement("input");
    tag.type = "hidden";
    tag.name = "draft_form";
    tag.value = name;
    form.appendChild(tag);

    var timer = null;
    // Listened for on the document, not on the form. A control joined to a
    // form by its form attribute -- the job line, whose fields sit in the
    // table because a form cannot -- sends its events up through where it
    // is, not to the form it belongs to, so a listener on the form heard
    // nothing and a job line was never kept. Asking each control which form
    // it belongs to covers both kinds. Change as well as input, because that
    // is what a select is sure to send.
    function edited(e) {
      if (!e.target || e.target.form !== form) return;
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
    }
    document.addEventListener("input", edited);
    document.addEventListener("change", edited);

    // Submitting is not saving. The draft used to be deleted here, on the
    // press, before anybody knew whether the server took it -- so a refusal,
    // a server error or a dropped connection left nothing to recover. It is
    // now deleted when the server says it saved it; see acknowledge.
    //
    // What does have to stop here is the autosave waiting to fire, or it
    // lands after the save and brings the submitted draft back.
    form.addEventListener("submit", function () {
      window.clearTimeout(timer);
    });
  }

  // The server's receipt. A handler that has saved a submission names its
  // draft in a short-lived cookie; the next page, or the offline queue on a
  // response, reads it and only then lets the draft go. Absent means not
  // saved -- a refusal, an error page, a connection that dropped -- and the
  // draft stays where it is.
  var SAVED = "freesms_saved";

  function acknowledge() {
    var m = document.cookie.match(/(?:^|;\s*)freesms_saved=([^;]*)/);
    if (!m) return;
    document.cookie = SAVED + "=; Max-Age=0; Path=/; SameSite=Lax";
    m[1].split(".").forEach(function (part) {
      var name;
      try { name = decodeURIComponent(part); } catch (e) { return; }
      if (!name) return;
      store.remove(draftKey(name));
      // The server dropped its copy when it saved; this is for an autosave
      // that was already on its way and arrived after it.
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

  // Who is signed in, from the page. Absent on the sign-in and setup pages,
  // which is how the queue knows not to send anything from there.
  function currentUser() {
    var m = document.querySelector('meta[name="freesms-user"]');
    return m ? m.getAttribute("content") : "";
  }

  // Names that must never reach browser storage, whatever form they are in.
  var SECRET = /pass|secret|token|credential/i;

  function sensitive(name) { return SECRET.test(name); }

  // Second line of defence, for a form marked offline by mistake: a form with
  // a password field in it is never held at all, and any field whose name
  // looks like a secret is dropped from what is stored.
  function hold(form) {
    if (form.querySelector('input[type="password"]')) return false;
    var data = new FormData(form);
    Array.from(data.keys()).forEach(function (name) {
      if (sensitive(name)) data.delete(name);
    });
    var body = new URLSearchParams(data).toString();
    var items = queue();
    // The person who queued it is part of the item. Work held on a shared
    // workshop tablet is sent as the person who did it, or not at all.
    items.push({ url: form.action, body: body, key: newKey(), at: Date.now(), user: currentUser() });
    setQueue(items);
    show();
    return true;
  }

  // Anything already stored by the old queue that carries a credential, or
  // was aimed at signing in or setting up, is removed without being sent.
  // Replaying a stored sign-in is pointless; keeping the password is the harm.
  function unsafe(it) {
    var path = "";
    try { path = new URL(it.url, location.origin).pathname; } catch (e) {}
    if (path === "/login" || path === "/setup" || path === "/logout") return true;
    return Array.from(new URLSearchParams(it.body || "").keys()).some(sensitive);
  }

  function purge() {
    // Queued work with nobody's name on it is kept for a person to look at,
    // never sent: there is no honest way to say whose it was.
    var held = queue(), owned = held.filter(function (it) { return it.user; });
    if (owned.length !== held.length) {
      var aside = refused();
      held.filter(function (it) { return !it.user; }).forEach(function (it) {
        aside.push({ url: it.url, body: it.body, status: "nobody's", at: it.at });
      });
      store.set(REFUSED, JSON.stringify(aside));
      store.set(QUEUE, JSON.stringify(owned));
    }

    // Drafts from before they were keyed by person. Whose they were cannot be
    // known, and the server holds its own copy per person, so the local one
    // goes.
    var uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\./i;
    forEachStored(function (key) {
      if (key.indexOf(DRAFT) === 0 && !uuid.test(key.slice(DRAFT.length))) store.remove(key);
    });

    [QUEUE, REFUSED].forEach(function (key) {
      var items;
      try { items = JSON.parse(store.get(key) || "[]"); } catch (e) { return; }
      var kept = items.filter(function (it) { return !unsafe(it); });
      if (kept.length !== items.length) store.set(key, JSON.stringify(kept));
    });
  }

  // Refused work is kept, not deleted. It used to be the only copy, and it
  // was thrown away with an alert -- after which there was nothing to recover
  // from. Now it waits for somebody to look at it and dismiss it on purpose.
  var REFUSED = "freesms.refused";

  function refused() {
    try { return JSON.parse(store.get(REFUSED) || "[]"); } catch (e) { return []; }
  }

  // Set while the server says the session has gone. Nothing is sent until
  // somebody signs in again; the queue is left exactly as it was.
  var needsSignIn = false;

  function show() {
    var bar = document.getElementById("held");
    if (!bar) return;
    var waiting = mine(queue()).length, bad = refused().length;
    bar.textContent = "";
    bar.hidden = waiting === 0 && bad === 0;
    if (bar.hidden) return;

    var parts = [];
    if (waiting) {
      parts.push(waiting + (waiting === 1 ? " thing is waiting to be sent" : " things are waiting to be sent") +
                 (needsSignIn ? (waiting === 1 ? " -- sign in again to send it." : " -- sign in again to send them.") : "."));
    }
    if (bad) {
      parts.push(bad + (bad === 1 ? " thing was refused by the server." : " things were refused by the server."));
    }
    bar.appendChild(document.createTextNode(parts.join(" ") + " "));

    if (bad) {
      var look = document.createElement("button");
      look.type = "button";
      look.className = "link";
      look.textContent = "Show and dismiss";
      look.addEventListener("click", function () {
        var items = refused();
        var text = items.map(function (it) {
          return it.status + "  " + it.url + "\n" + decodeURIComponent(it.body.replace(/\+/g, " "));
        }).join("\n\n");
        // A confirm, not a silent delete: this is the last copy of what
        // somebody typed, and they decide when it is gone.
        if (window.confirm("Refused by the server:\n\n" + text + "\n\nDismiss these?")) {
          store.remove(REFUSED);
          show();
        }
      });
      bar.appendChild(look);
    }
  }

  // Only this person's work. An item queued before items carried a person
  // used to be adopted by whoever happened to be signed in, which sent one
  // person's work under another's name; those are set aside at start-up
  // instead, by purge.
  function mine(items) {
    var me = currentUser();
    return items.filter(function (it) { return it.user && it.user === me; });
  }

  function landedOn(resp, path) {
    try { return new URL(resp.url).pathname === path; } catch (e) { return false; }
  }

  function flush() {
    // Nobody signed in -- the sign-in page, the setup page, an expired
    // session -- means nothing is sent. It used to flush from the sign-in
    // page too.
    if (!currentUser() || needsSignIn) { show(); return; }

    var all = queue();
    var next = mine(all)[0];
    if (!next) { show(); return; }

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
      // The session has gone. Stop, keep everything, and say why. A
      // followed redirect to the sign-in page answers 200, which is how this
      // used to be read as success and the work discarded.
      if (resp.status === 401 || landedOn(resp, "/login") || landedOn(resp, "/setup")) {
        needsSignIn = true;
        show();
        return;
      }
      if (resp.status >= 500) { show(); return; } // Try again later.

      acknowledge();
      var rest = queue().filter(function (it) { return it.key !== next.key; });
      if (!resp.ok) {
        // Refused: kept, with what the server said, for a person to look at.
        var bad = refused();
        bad.push({ url: next.url, body: next.body, status: resp.status, at: Date.now() });
        store.set(REFUSED, JSON.stringify(bad));
      }
      setQueue(rest);
      show();
      if (mine(rest).length) flush();
    }).catch(function () {
      // Still offline.
      show();
    });
  }

  // Which forms may be held for later is an allowlist, marked in the
  // template with data-offline, not a blocklist. Every POST form used to be
  // held, including sign-in and setup, and the queue stored the whole body --
  // password included -- in plain text in localStorage. A blocklist forgets
  // the next form somebody adds; an allowlist forgets nothing it was not
  // told about.
  function guard(form) {
    var offline = form.hasAttribute("data-offline");
    form.addEventListener("submit", function (e) {
      if (navigator.onLine !== false) return;
      e.preventDefault();
      if (!offline || !hold(form)) {
        // Not something that can wait. The page stays as it is, so what
        // was typed is still in front of the person.
        window.alert("This needs a connection. Nothing has been sent and nothing has been stored.");
        return;
      }
      window.alert("No connection. This is being held and will be sent when there is one.");
    });
  }

  function forEachStored(fn) {
    var keys = [];
    try {
      for (var i = 0; i < window.localStorage.length; i++) keys.push(window.localStorage.key(i));
    } catch (e) { return; }
    keys.forEach(fn);
  }

  function forgetMyDrafts() {
    var mineKey = DRAFT + currentUser() + ".";
    forEachStored(function (key) {
      if (key.indexOf(mineKey) === 0) store.remove(key);
    });
  }

  // ---- Wiring -------------------------------------------------------------

  document.addEventListener("DOMContentLoaded", function () {
    acknowledge();
    Array.prototype.forEach.call(document.querySelectorAll("form[data-keep]"), watch);
    purge();
    Array.prototype.forEach.call(document.querySelectorAll("form[action='/logout']"), function (form) {
      form.addEventListener("submit", forgetMyDrafts);
    });
    Array.prototype.forEach.call(document.querySelectorAll("form[method='post']"), guard);
    show();
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
