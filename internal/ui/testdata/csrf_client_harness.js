// Behavioural harness for internal/ui/csrf_client.js, run by
// csrf_client_test.go under node against a minimal DOM shim.
// 2026-09-19-ce-human-login-design.md §6.3. Exits non-zero on the first
// failed assertion, printing which request shape broke.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const ORIGIN = 'https://swarms.vornik.io';
const TOK = 'Zm9vYmFyYmF6cXV4LXRva2VuLXZhbHVlLTEyMzQ1Njc4OQ';

// ---- minimal DOM ---------------------------------------------------------
class Node_ {
  constructor(tag) { this.tagName = tag.toUpperCase(); this.attrs = {}; this.children = []; this.parentNode = null; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null; }
  get firstChild() { return this.children[0] || null; }
  appendChild(el) { return this.insertBefore(el, null); }
  insertBefore(el, ref) {
    if (el.parentNode) el.parentNode.removeChild(el);
    const i = ref ? this.children.indexOf(ref) : -1;
    if (i < 0) this.children.push(el); else this.children.splice(i, 0, el);
    el.parentNode = this;
    return el;
  }
  removeChild(el) {
    const i = this.children.indexOf(el);
    if (i >= 0) this.children.splice(i, 1);
    el.parentNode = null;
    return el;
  }
  querySelector(sel) {
    const m = /^input\[name="([^"]+)"\]$/.exec(sel);
    if (!m) throw new Error('shim: unsupported selector ' + sel);
    return this.children.find((c) => c.tagName === 'INPUT' && c.name === m[1]) || null;
  }
}
class Input_ extends Node_ { constructor() { super('input'); this.name = ''; this.value = ''; this.type = 'text'; } }

const submitted = [];
class HTMLFormElement extends Node_ {
  constructor(attrs) { super('form'); for (const k of Object.keys(attrs || {})) this.setAttribute(k, attrs[k]); }
  submit() { submitted.push(this.children.map((c) => [c.name, c.value])); }
  // requestSubmit fires a submit event (unlike submit()); the browser then
  // submits natively, which the shim records the same way.
  requestSubmit(submitter) {
    dispatch('submit', { target: this, submitter: submitter || null });
    submitted.push(this.children.map((c) => [c.name, c.value]));
  }
}

const listeners = {};
const document = {
  cookie: '',
  baseURI: ORIGIN + '/ui/tasks/t1',
  addEventListener(type, fn) { (listeners[type] = listeners[type] || []).push(fn); },
  createElement(tag) {
    if (tag.toLowerCase() === 'input') return new Input_();
    return new Node_(tag);
  },
};
function dispatch(type, ev) { for (const fn of listeners[type] || []) fn(ev); }

const fetchCalls = [];
const ctx = {
  document,
  location: { href: ORIGIN + '/ui/tasks/t1', origin: ORIGIN },
  HTMLFormElement,
  URL, Headers, Request,
  fetch: (input, init) => { fetchCalls.push({ input, init }); return Promise.resolve(null); },
  console,
};
ctx.window = ctx;
vm.createContext(ctx);
vm.runInContext(fs.readFileSync(process.argv[2], 'utf8'), ctx);
// Loading twice must not double-wrap (a page can include pageHead once, but
// a partial rendered twice must not stack wrappers).
vm.runInContext(fs.readFileSync(process.argv[2], 'utf8'), ctx);

function headerOf(call) {
  let h = call.init && call.init.headers;
  if (!h && call.input instanceof Request) h = call.input.headers;
  if (!h) return null;
  return new Headers(h).get('X-Vornik-CSRF');
}
function lastFetch() { return fetchCalls[fetchCalls.length - 1]; }
function fieldOf(form) { const c = form.firstChild; return c && c.name === 'vornik_csrf' ? c.value : null; }
function countFields(form) { return form.children.filter((c) => c.name === 'vornik_csrf').length; }

function run(name, fn) {
  try { fn(); } catch (e) { console.error('FAIL ' + name + ': ' + e.message); process.exit(1); }
  console.log('ok   ' + name);
}

// ---- no cookie: nothing is ever attached --------------------------------
run('no cookie: htmx attaches nothing', () => {
  const d = { verb: 'post', path: '/ui/tasks/t1/pause', headers: {} };
  dispatch('htmx:configRequest', { detail: d });
  assert.equal(d.headers['X-Vornik-CSRF'], undefined);
});
run('no cookie: fetch attaches nothing', () => {
  ctx.fetch('/api/v1/x', { method: 'POST' });
  assert.equal(headerOf(lastFetch()), null);
});
run('no cookie: form gets no field', () => {
  const f = new HTMLFormElement({ method: 'POST', action: '/ui/tasks/t1/pause' });
  dispatch('submit', { target: f, submitter: null });
  assert.equal(countFields(f), 0);
});

document.cookie = 'theme=dark; vornik_csrf=' + TOK + '; vornik_session_ui=admin';

// ---- htmx ---------------------------------------------------------------
for (const verb of ['post', 'put', 'patch', 'delete']) {
  run('htmx ' + verb + ' same-origin gets the header', () => {
    const d = { verb, path: '/ui/tasks/t1/pause', headers: {} };
    dispatch('htmx:configRequest', { detail: d });
    assert.equal(d.headers['X-Vornik-CSRF'], TOK);
  });
}
run('htmx get gets no header', () => {
  const d = { verb: 'get', path: '/ui/tasks', headers: {} };
  dispatch('htmx:configRequest', { detail: d });
  assert.equal(d.headers['X-Vornik-CSRF'], undefined);
});
run('htmx cross-origin post gets no header', () => {
  const d = { verb: 'post', path: 'https://evil.example/x', headers: {} };
  dispatch('htmx:configRequest', { detail: d });
  assert.equal(d.headers['X-Vornik-CSRF'], undefined);
});

// ---- fetch --------------------------------------------------------------
run('fetch POST relative, plain-object headers kept', () => {
  ctx.fetch('/api/v1/executions/e/hints', { method: 'POST', headers: { 'Content-Type': 'application/json' } });
  const c = lastFetch();
  assert.equal(headerOf(c), TOK);
  assert.equal(new Headers(c.init.headers).get('Content-Type'), 'application/json');
});
run('fetch lowercase method', () => {
  ctx.fetch('/x', { method: 'delete' });
  assert.equal(headerOf(lastFetch()), TOK);
});
run('fetch array headers', () => {
  ctx.fetch('/x', { method: 'PATCH', headers: [['Accept', 'text/html']] });
  const c = lastFetch();
  assert.equal(headerOf(c), TOK);
  assert.equal(new Headers(c.init.headers).get('Accept'), 'text/html');
});
run('fetch Headers instance', () => {
  ctx.fetch('/x', { method: 'PUT', headers: new Headers({ 'X-Other': '1' }) });
  const c = lastFetch();
  assert.equal(headerOf(c), TOK);
  assert.equal(new Headers(c.init.headers).get('X-Other'), '1');
});
run('fetch Request object same-origin', () => {
  ctx.fetch(new Request(ORIGIN + '/y', { method: 'POST', headers: { 'X-Keep': 'k' } }));
  const c = lastFetch();
  assert.equal(headerOf(c), TOK);
  assert.equal(new Headers(c.init ? c.init.headers : c.input.headers).get('X-Keep'), 'k');
});
run('fetch URL object same-origin', () => {
  ctx.fetch(new URL('/z', ORIGIN), { method: 'POST' });
  assert.equal(headerOf(lastFetch()), TOK);
});
run('fetch GET gets no header', () => {
  ctx.fetch('/ui/palette/search?q=a');
  assert.equal(headerOf(lastFetch()), null);
});
run('fetch cross-origin POST gets no header', () => {
  ctx.fetch('https://evil.example/collect', { method: 'POST' });
  assert.equal(headerOf(lastFetch()), null);
});
run('fetch protocol-relative cross-origin gets no header', () => {
  ctx.fetch('//evil.example/collect', { method: 'POST' });
  assert.equal(headerOf(lastFetch()), null);
});

// ---- plain forms ----------------------------------------------------------
run('form POST submit: token is the FIRST field', () => {
  const f = new HTMLFormElement({ method: 'POST', action: '/ui/tasks/t1/pause' });
  const reason = new Input_(); reason.name = 'reason'; reason.value = 'x'; f.appendChild(reason);
  dispatch('submit', { target: f, submitter: null });
  assert.equal(fieldOf(f), TOK);
  assert.equal(f.children[1], reason);
});
run('form with no action attribute posts to the page itself', () => {
  const f = new HTMLFormElement({ method: 'post' });
  dispatch('submit', { target: f, submitter: null });
  assert.equal(fieldOf(f), TOK);
});
run('form GET gets no field (a token must never reach a URL)', () => {
  const f = new HTMLFormElement({ method: 'GET', action: '/ui/tasks' });
  dispatch('submit', { target: f, submitter: null });
  assert.equal(countFields(f), 0);
});
run('form without method (htmx hx-post form) gets no field', () => {
  const f = new HTMLFormElement({ 'hx-post': '/ui/tasks/t1/approve' });
  dispatch('submit', { target: f, submitter: null });
  assert.equal(countFields(f), 0);
});
run('form POST cross-origin action gets no field', () => {
  const f = new HTMLFormElement({ method: 'POST', action: 'https://evil.example/x' });
  dispatch('submit', { target: f, submitter: null });
  assert.equal(countFields(f), 0);
});
run('submitter formaction cross-origin removes an injected field', () => {
  const f = new HTMLFormElement({ method: 'POST', action: '/ui/tasks-bulk/cancel' });
  dispatch('submit', { target: f, submitter: null });
  assert.equal(countFields(f), 1);
  const b = new Node_('button'); b.setAttribute('formaction', 'https://evil.example/x');
  dispatch('submit', { target: f, submitter: b });
  assert.equal(countFields(f), 0);
});
run('submitter formaction same-origin keeps the field', () => {
  const f = new HTMLFormElement({ id: 'bulk-form', method: 'POST' });
  const b = new Node_('button'); b.setAttribute('formaction', '/ui/tasks-bulk/retry');
  dispatch('submit', { target: f, submitter: b });
  assert.equal(fieldOf(f), TOK);
});
run('submitter formmethod=get removes the field', () => {
  const f = new HTMLFormElement({ method: 'POST', action: '/ui/x' });
  dispatch('submit', { target: f, submitter: null });
  const b = new Node_('button'); b.setAttribute('formmethod', 'get');
  dispatch('submit', { target: f, submitter: b });
  assert.equal(countFields(f), 0);
});
run('an existing field is refreshed on rotation, never duplicated', () => {
  const f = new HTMLFormElement({ method: 'POST', action: '/ui/x' });
  dispatch('submit', { target: f, submitter: null });
  document.cookie = 'vornik_csrf=rotated-token';
  dispatch('submit', { target: f, submitter: null });
  assert.equal(countFields(f), 1);
  assert.equal(fieldOf(f), 'rotated-token');
  document.cookie = 'theme=dark; vornik_csrf=' + TOK;
});
run('programmatic form.submit() injects before submitting', () => {
  const f = new HTMLFormElement({ method: 'POST', action: '/ui/tasks/t1/steer' });
  const n = submitted.length;
  f.submit();
  assert.equal(submitted.length, n + 1, 'the original submit ran exactly once');
  assert.deepEqual(submitted[n][0], ['vornik_csrf', TOK]);
});
run('programmatic submit of a GET form carries no token', () => {
  const f = new HTMLFormElement({ method: 'GET', action: '/ui/insights' });
  const n = submitted.length;
  f.submit();
  assert.equal(submitted.length, n + 1);
  assert.equal(submitted[n].length, 0);
});
run('requestSubmit() is covered by the submit listener', () => {
  const f = new HTMLFormElement({ method: 'POST', action: '/ui/tasks/t1/pause' });
  const n = submitted.length;
  f.requestSubmit();
  assert.equal(submitted.length, n + 1);
  assert.deepEqual(submitted[n][0], ['vornik_csrf', TOK]);
});
run('a pre-existing vornik_csrf field is overwritten and moved first, never duplicated', () => {
  const f = new HTMLFormElement({ method: 'POST', action: '/ui/x' });
  const a = new Input_(); a.name = 'a'; a.value = '1'; f.appendChild(a);
  const stale = new Input_(); stale.name = 'vornik_csrf'; stale.value = 'stale'; f.appendChild(stale);
  dispatch('submit', { target: f, submitter: null });
  assert.equal(countFields(f), 1);
  assert.equal(fieldOf(f), TOK);
});
run('the caller\'s Request and init objects are not mutated', () => {
  const req = new Request(ORIGIN + '/y', { method: 'POST' });
  const init = { method: 'POST', headers: { 'X-A': '1' } };
  ctx.fetch(req);
  ctx.fetch('/y', init);
  assert.equal(req.headers.get('X-Vornik-CSRF'), null);
  assert.deepEqual(init.headers, { 'X-A': '1' });
});
run('relative targets resolve against document.baseURI', () => {
  // A <base href> elsewhere would move relative targets; the sender must
  // decide with the same base the browser uses.
  document.baseURI = 'https://evil.example/';
  ctx.fetch('/relative', { method: 'POST' });
  assert.equal(headerOf(lastFetch()), null, 'a relative URL under a cross-origin base is cross-origin');
  document.baseURI = ORIGIN + '/ui/tasks/t1';
  ctx.fetch('/relative', { method: 'POST' });
  assert.equal(headerOf(lastFetch()), TOK);
});
console.log('all ok');
