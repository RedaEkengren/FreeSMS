// Runs keep.js against the smallest DOM it needs and reports what it did.
//
// Not jsdom: a dependency to fetch and keep up to date, for a test that needs
// a form, a cookie, local storage and a clock. Driven by keep_test.go, which
// passes the scenario as the first argument and reads one JSON line back.
"use strict";
const fs = require("fs");
const vm = require("vm");

const USER = "11111111-1111-1111-1111-111111111111";

function page({ cookie = "", storage = {}, form = "intake", value = "" }) {
  const listeners = {};
  const fetches = [];
  const timers = [];
  const local = new Map(Object.entries(storage));
  let jar = cookie;

  const field = { name: "complaint", type: "textarea", value };
  const appended = [];
  const formEl = {
    action: "/jobs/new",
    elements: Object.assign([field], { complaint: field }),
    listeners: {},
    getAttribute: (n) => (n === "data-keep" ? form : n === "method" ? "post" : null),
    hasAttribute: () => false,
    querySelector: () => null,
    appendChild: (el) => { appended.push(el); formEl.elements.push(el); },
    addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); },
    fire(type) { (this.listeners[type] || []).forEach((fn) => fn({ preventDefault() {} })); },
  };

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
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(null) });
    },
  };
  vm.createContext(sandbox);
  vm.runInContext(fs.readFileSync(process.argv[3], "utf8"), sandbox);
  (listeners.DOMContentLoaded || []).forEach((fn) => fn());

  return {
    form: formEl, field, fetches, appended,
    type(text) { field.value = text; formEl.fire("input"); },
    runTimers() { timers.splice(0).forEach((fn) => fn && fn()); },
    draft: () => window.localStorage.getItem("freesms.draft." + USER + "." + form),
    cookie: () => jar,
  };
}

const writes = (p, method) => p.fetches.filter((f) => f.method === method && f.url.startsWith("/drafts"));

const scenarios = {
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

process.stdout.write(JSON.stringify(scenarios[process.argv[2]]()) + "\n");
