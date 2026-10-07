// Runs keep.js against the smallest DOM it needs and reports what it did.
//
// Not jsdom: a dependency to fetch and keep up to date, for a test that needs
// a form, a cookie, local storage and a clock. Driven by keep_test.go, which
// passes the scenario as the first argument and reads one JSON line back.
"use strict";
const fs = require("fs");
const vm = require("vm");

const USER = "11111111-1111-1111-1111-111111111111";

function page({ cookie = "", storage = {}, form = "intake", value = "", server = null, qty = "1" }) {
  const listeners = {};
  const fetches = [];
  const timers = [];
  const local = new Map(Object.entries(storage));
  let jar = cookie;

  const field = { name: "complaint", type: "textarea", value, defaultValue: "" };
  // A field the page sends with a value already in it, like a quantity of 1.
  const quantity = { name: "quantity", type: "text", value: qty, defaultValue: "1" };
  // A control joined to the form by its form attribute rather than sitting
  // inside it, the way the job line's fields sit in a table.
  const outside = { name: "kind", type: "select-one", value: "", defaultValue: "" };
  // And one belonging to some other form on the same page.
  const stranger = { name: "q", type: "text", value: "", form: {} };
  const appended = [];
  const formEl = {
    action: "/jobs/new",
    elements: Object.assign([field, quantity, outside], { complaint: field, quantity, kind: outside }),
    listeners: {},
    getAttribute: (n) => (n === "data-keep" ? form : n === "method" ? "post" : null),
    hasAttribute: () => false,
    querySelector: () => null,
    appendChild: (el) => { appended.push(el); formEl.elements.push(el); },
    addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); },
    fire(type) { (this.listeners[type] || []).forEach((fn) => fn({ preventDefault() {} })); },
  };

  field.form = formEl;
  outside.form = formEl;

  const document = {
    get cookie() { return jar; },
    set cookie(v) {
      // Enough of a cookie jar for one name: Max-Age=0 removes it.
      const [pair] = v.split(";");
      const [k] = pair.split("=");
      if (/max-age=0/i.test(v)) {
        jar = jar.split(/;\s*/).filter((c) => c && !c.startsWith(k + "=")).join("; ");
      } else {
        jar = pair;
      }
    },
    addEventListener: (type, fn) => { (listeners[type] ||= []).push(fn); },
    querySelector: (sel) =>
      sel === 'meta[name="freesms-user"]' ? { getAttribute: () => USER } : null,
    querySelectorAll: (sel) => (sel === "form[data-keep]" ? [formEl] : []),
    getElementById: () => null,
    createElement: () => ({}),
  };

  const window = {
    localStorage: {
      getItem: (k) => (local.has(k) ? local.get(k) : null),
      setItem: (k, v) => local.set(k, String(v)),
      removeItem: (k) => local.delete(k),
      key: (i) => Array.from(local.keys())[i],
      get length() { return local.size; },
    },
    setTimeout: (fn) => { timers.push(fn); return timers.length; },
    clearTimeout: (id) => { if (id) timers[id - 1] = null; },
    addEventListener() {},
    alert() {},
    crypto: { randomUUID: () => "k" },
  };

  const sandbox = {
    window, document, console,
    navigator: { onLine: true },
    location: { origin: "http://localhost" },
    URL, URLSearchParams,
    FormData: class { keys() { return [][Symbol.iterator](); } },
    setTimeout: window.setTimeout,
    fetch: (url, opts = {}) => {
      fetches.push({ url, method: opts.method || "GET" });
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(server) });
    },
  };
  vm.createContext(sandbox);
  vm.runInContext(fs.readFileSync(process.argv[3], "utf8"), sandbox);
  (listeners.DOMContentLoaded || []).forEach((fn) => fn());

  return {
    form: formEl, field, fetches, appended,
    // Events go up through where a control is in the page. One inside the
    // form passes the form on its way to the document; one joined by the
    // form attribute, or in another form, never reaches this form at all.
    type(text, which = "inside", type = "input") {
      const el = { inside: field, outside, stranger }[which];
      el.value = text;
      const e = { target: el, preventDefault() {} };
      if (which === "inside") (formEl.listeners[type] || []).forEach((fn) => fn(e));
      (listeners[type] || []).forEach((fn) => fn(e));
    },
    runTimers() { timers.splice(0).forEach((fn) => fn && fn()); },
    quantity,
    draft: () => window.localStorage.getItem("freesms.draft." + USER + "." + form),
    cookie: () => jar,
  };
}


// A form submitted with no connection, and what the queue made of it.
//
// The inspection item's answer is the button pressed; the photograph's is a
// file. FormData here does what the browser's does: the form's own controls,
// never the button that submitted it.
function offline({ named = true, file = false, submitter, words }) {
  const listeners = {};
  const local = new Map();
  const alerts = [];
  // #held as the layout renders it: the banner, carrying the queue's words
  // in the page's language. Absent, as on a page cached before it had them.
  const bar = words && {
    text: "", hidden: true,
    getAttribute: (n) => (n in words ? words[n] : null),
    set textContent(v) { this.text = v; },
    appendChild(node) { this.text += node.text || ""; },
  };
  const note = { name: "note", type: "text", value: "Worn to the indicator" };
  const photo = { name: "photo", type: "file", value: "C:\\fakepath\\brake.jpg" };
  const controls = file ? [note, photo] : [note];
  const formEl = {
    tagName: "FORM",
    action: "http://localhost/inspections/i/items/1",
    elements: controls,
    listeners: {},
    getAttribute: (n) => (n === "method" ? "post" : n === "enctype" && file ? "multipart/form-data" : null),
    hasAttribute: (n) => n === "data-offline",
    querySelector: (sel) =>
      sel.includes("file") ? (file ? photo : null) :
      sel.includes("button[name]") ? (named ? {} : null) : null,
    addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); },
  };
  const document = {
    cookie: "",
    addEventListener: (type, fn) => { (listeners[type] ||= []).push(fn); },
    querySelector: (sel) =>
      sel === 'meta[name="freesms-user"]' ? { getAttribute: () => USER } : null,
    querySelectorAll: () => [],
    getElementById: (id) => (id === "held" && bar) || null,
    createElement: () => ({ addEventListener() {} }),
    createTextNode: (text) => ({ text }),
  };
  const window = {
    localStorage: {
      getItem: (k) => (local.has(k) ? local.get(k) : null),
      setItem: (k, v) => local.set(k, String(v)),
      removeItem: (k) => local.delete(k),
      key: (i) => Array.from(local.keys())[i],
      get length() { return local.size; },
    },
    setTimeout() {}, clearTimeout() {}, addEventListener() {},
    alert: (m) => alerts.push(m),
    crypto: { randomUUID: () => "k" },
  };
  class FakeFormData {
    constructor(form) { this.pairs = form.elements.map((el) => [el.name, el.type === "file" ? "[object File]" : el.value]); }
    append(k, v) { this.pairs.push([k, v]); }
    delete(k) { this.pairs = this.pairs.filter(([n]) => n !== k); }
    keys() { return this.pairs.map(([k]) => k)[Symbol.iterator](); }
    [Symbol.iterator]() { return this.pairs[Symbol.iterator](); }
  }
  const sandbox = {
    window, document, console, URL, URLSearchParams,
    navigator: { onLine: false },
    location: { origin: "http://localhost" },
    FormData: FakeFormData,
    fetch: () => Promise.reject(new Error("offline")),
  };
  vm.createContext(sandbox);
  vm.runInContext(fs.readFileSync(process.argv[3], "utf8"), sandbox);
  (listeners.DOMContentLoaded || []).forEach((fn) => fn());

  let prevented = false;
  // The submission reaches the form's own listeners and then, bubbling, the
  // document's, as a browser sends it.
  const e = { target: formEl, submitter, preventDefault() { prevented = true; } };
  (formEl.listeners.submit || []).forEach((fn) => fn(e));
  (listeners.submit || []).forEach((fn) => fn(e));
  const queue = JSON.parse(window.localStorage.getItem("freesms.queue") || "[]");
  return { prevented, held: queue.map((it) => it.body), alerts: alerts.length, said: alerts, banner: bar ? bar.text.trim() : "" };
}

const writes = (p, method) => p.fetches.filter((f) => f.method === method && f.url.startsWith("/drafts"));

const scenarios = {
  // Pressing "fail" with no connection.
  pressed: () => offline({ submitter: { name: "status", value: "fail" } }),
  // The same, on a Swedish page: the words come from #held.
  swedish: () => offline({
    submitter: { name: "status", value: "fail" },
    words: {
      "data-held": "Ingen anslutning. Det här hålls och skickas när det finns en anslutning.",
      "data-waiting-one": "%d sak väntar på att skickas.",
      "data-waiting-other": "%d saker väntar på att skickas.",
    },
  }),
  // The same form where the browser does not say which button was pressed.
  unknownButton: () => offline({ submitter: undefined }),
  // A photograph with no connection.
  photo: () => offline({ named: false, file: true, submitter: { name: "", value: "" } }),
  // The server's copy arriving on a page with nothing local: it fills what
  // still holds what the page came with, and nothing the person has changed.
  async fromServer() {
    const fresh = page({ server: { complaint: "Rattles", quantity: "1,5" } });
    const typed = page({ server: { complaint: "Rattles", quantity: "1,5" }, qty: "3" });
    await new Promise((r) => setImmediate(r));
    return { fresh: { complaint: fresh.field.value, quantity: fresh.quantity.value },
             typed: { quantity: typed.quantity.value } };
  },
  // Each kind of control, typed into, and what was kept.
  edits() {
    const kept = (p) => { p.runTimers(); return { local: p.draft(), saves: writes(p, "POST").length }; };
    const inside = page({}); inside.type("Rattles");
    const outside = page({}); outside.type("part", "outside", "change");
    const stranger = page({}); stranger.type("brake pads", "stranger");
    return { inside: kept(inside), outside: kept(outside), stranger: kept(stranger) };
  },
  // Pressing submit, before the server has said anything.
  submit() {
    const p = page({});
    p.type("Rattles at 80");
    p.form.fire("submit");
    p.runTimers();
    return {
      draft: p.draft(),
      deletes: writes(p, "DELETE").length,
      saves: writes(p, "POST").length,
      tagged: p.appended.some((el) => el.name === "draft_form" && el.value === "intake" && el.type === "hidden"),
    };
  },
  // The next page, after the server saved it.
  receipt() {
    const key = "freesms.draft." + USER + ".intake";
    const p = page({ cookie: "other=1; freesms_saved=shop-details.intake", storage: { [key]: '{"complaint":"x"}' } });
    return { draft: p.draft(), deletes: writes(p, "DELETE").map((f) => f.url), cookie: p.cookie() };
  },
  // The next page, after a refusal: no receipt.
  refusal() {
    const key = "freesms.draft." + USER + ".intake";
    const p = page({ storage: { [key]: '{"complaint":"x"}' } });
    return { draft: p.draft(), deletes: writes(p, "DELETE").length };
  },
};

Promise.resolve(scenarios[process.argv[2]]()).then((out) => process.stdout.write(JSON.stringify(out) + "\n"));
