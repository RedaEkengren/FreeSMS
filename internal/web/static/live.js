/*
 * Screens that keep themselves up to date.
 *
 * A part of a page that shows something other people change is marked
 *
 *   <div id="live-board" data-live="board">
 *
 * with an id and the topics it shows ("board", "parts", "calendar",
 * "job:<id>", several separated by spaces). The server says on /events,
 * as server-sent events, when a topic in the workshop changed -- only that it
 * changed, never what to. The page then asks for itself again, exactly as a
 * reload would and through the same permission checks, and swaps in the
 * marked parts that differ.
 *
 * A part somebody is using is left alone: a field with the cursor in it, or
 * one that has been typed into, is never overwritten. It is brought up to
 * date once they are done with it.
 *
 * Where the stream cannot be held -- an old browser, a proxy that buffers,
 * a server with too many open screens -- the page asks on an interval
 * instead. Either way it is never long out of date.
 */
(function () {
  "use strict";

  var POLL = 30000;     // ms between refreshes without a stream
  var SETTLE = 300;     // ms to gather a burst of changes into one refresh
  var HEADER = "X-FreeSMS-Live";

  function regions(root) {
    return Array.prototype.slice.call((root || document).querySelectorAll("[data-live][id]"));
  }

  function topicsOf(selector, attr) {
    var all = {};
    Array.prototype.forEach.call(document.querySelectorAll(selector), function (el) {
      el.getAttribute(attr).split(/\s+/).forEach(function (t) { if (t) all[t] = true; });
    });
    return all;
  }

  // Somebody is in it: the cursor is in a field, or a field holds something
  // other than what the page was loaded with.
  function inUse(region) {
    var active = document.activeElement;
    if (active && active !== document.body && region.contains(active) &&
        /^(INPUT|SELECT|TEXTAREA)$/.test(active.tagName)) return true;
    var fields = region.querySelectorAll("input, select, textarea");
    for (var i = 0; i < fields.length; i++) {
      var f = fields[i];
      if (f.type === "hidden" || f.readOnly) continue;
      if (f.type === "checkbox" || f.type === "radio") {
        if (f.checked !== f.defaultChecked) return true;
      } else if (f.tagName === "SELECT") {
        // With no option marked selected, a select starts on its first --
        // which then is not "defaultSelected", and a select nobody touched
        // used to read as changed.
        var start = 0;
        for (var j = 0; j < f.options.length; j++) {
          if (f.options[j].defaultSelected) { start = j; break; }
        }
        if (f.selectedIndex !== start) return true;
      } else if (f.value !== f.defaultValue) {
        return true;
      }
    }
    return false;
  }

  var waiting = false;   // a region was left alone and still needs bringing up to date
  var running = false;
  var again = false;
  var settle = null;

  function swap(fresh) {
    regions().forEach(function (old) {
      var next = fresh.getElementById(old.id);
      if (!next || next.innerHTML === old.innerHTML) return;
      if (inUse(old)) { waiting = true; return; }
      // What somebody opened stays open.
      var opened = Array.prototype.map.call(old.querySelectorAll("details"), function (d) { return d.open; });
      old.innerHTML = next.innerHTML;
      Array.prototype.forEach.call(old.querySelectorAll("details"), function (d, i) {
        if (opened[i]) d.open = true;
      });
      if (window.htmx) window.htmx.process(old);
      old.dispatchEvent(new CustomEvent("freesms:refreshed", { bubbles: true }));
    });
  }

  function refresh() {
    // A page whose only live part is the header has nothing to fetch.
    if (!regions().length) return;
    if (running) { again = true; return; }
    running = true;
    waiting = false;
    fetch(window.location.href, { headers: (function () { var h = {}; h[HEADER] = "1"; return h; })(), credentials: "same-origin" })
      .then(function (resp) {
        // Signed out while the page was open: say so the way any page does.
        if (resp.redirected && new URL(resp.url).pathname === "/login") {
          window.location.assign("/login");
          return null;
        }
        if (!resp.ok) return null;
        return resp.text();
      })
      .then(function (html) {
        if (html) swap(new DOMParser().parseFromString(html, "text/html"));
      })
      .catch(function () { /* No connection: the next change or poll tries again. */ })
      .then(function () {
        running = false;
        if (again) { again = false; soon(); }
      });
  }

  function soon() {
    window.clearTimeout(settle);
    settle = window.setTimeout(refresh, SETTLE);
  }

  var polling = null;
  function poll() {
    if (polling) return;
    polling = window.setInterval(function () { refresh(); tell(); }, POLL);
  }

  // Something that is not a part of the page but cares -- the header's
  // count of what is waiting -- is told with an event, and asks for itself.
  function tell() {
    document.body.dispatchEvent(new CustomEvent("freesms:change"));
  }

  function listen(topics, others) {
    if (!window.EventSource) { poll(); return; }
    var stream = new EventSource("/events");
    var dropped = false;
    stream.addEventListener("change", function (e) {
      if (e.data === "*" || topics[e.data]) soon();
      if (e.data === "*" || others[e.data]) tell();
    });
    stream.addEventListener("signedout", function () {
      stream.close();
      window.location.assign("/login");
    });
    stream.addEventListener("open", function () {
      // Back after a drop: whatever happened meanwhile was not heard.
      if (dropped) { dropped = false; soon(); tell(); }
    });
    stream.addEventListener("error", function () {
      dropped = true;
      // The browser reconnects by itself unless the server refused the
      // stream outright; then this page polls instead.
      if (stream.readyState === EventSource.CLOSED) poll();
    });
  }

  // A sound and a buzz when more is waiting than a moment ago -- only for a
  // person who turned it on (data-sound), and never for what was already
  // waiting when the page opened.
  var waitingWas = null;
  function chime() {
    var count = document.getElementById("mine-count");
    if (!count) return;
    var n = parseInt(count.getAttribute("data-n"), 10) || 0;
    if (waitingWas !== null && n > waitingWas && count.hasAttribute("data-sound")) {
      try {
        var Ctx = window.AudioContext || window.webkitAudioContext;
        if (Ctx) {
          var ctx = new Ctx(), tone = ctx.createOscillator(), gain = ctx.createGain();
          tone.frequency.value = 880;
          gain.gain.setValueAtTime(0.15, ctx.currentTime);
          gain.gain.exponentialRampToValueAtTime(0.001, ctx.currentTime + 0.25);
          tone.connect(gain).connect(ctx.destination);
          tone.start();
          tone.stop(ctx.currentTime + 0.25);
        }
      } catch (e) { /* A browser that will not play before a touch: silence. */ }
      if (navigator.vibrate) navigator.vibrate(150);
    }
    waitingWas = n;
  }

  document.addEventListener("DOMContentLoaded", function () {
    var topics = topicsOf("[data-live][id]", "data-live");
    var others = topicsOf("[data-live-topics]", "data-live-topics");
    if (!Object.keys(topics).length && !Object.keys(others).length) return;
    listen(topics, others);
    document.body.addEventListener("htmx:afterSettle", chime);

    // A phone that slept heard nothing; it catches up when it is looked at.
    document.addEventListener("visibilitychange", function () {
      if (document.visibilityState === "visible") { soon(); tell(); }
    });
    // A part left alone because somebody was in it is brought up to date
    // once they leave it.
    document.addEventListener("focusout", function () {
      if (waiting) soon();
    });
  });
})();
