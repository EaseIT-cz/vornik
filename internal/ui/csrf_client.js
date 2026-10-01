// Vornik console: the ONE sender of the double-submit CSRF token.
// Design: https://docs.vornik.io §6.3.
// Rendered inline by the shared pageHead partial (template func
// csrfClientScript), so it runs on every page before htmx or any page script.
// Inline, not /ui/static/: the service worker serves static assets
// cache-first, so a changed static sender would never reach a cached browser.
//
// It reads the vornik_csrf cookie AT REQUEST TIME (the cookie is readable by
// design; another origin cannot read it) and attaches it only to SAME-ORIGIN
// mutating requests, by four paths: htmx (header), fetch (header), a plain
// <form method=post> (hidden FIRST field, written at submit time), and
// programmatic form.submit() (same). No cookie → nothing is attached.
(function () {
    'use strict';
    var w = window;
    if (w.__vornikCSRFSender) return; // installed once, never stacked
    w.__vornikCSRFSender = true;

    var COOKIE = 'vornik_csrf', HEADER = 'X-Vornik-CSRF', FIELD = 'vornik_csrf';

    function token() {
        var parts = (document.cookie || '').split(';');
        for (var i = 0; i < parts.length; i++) {
            var p = parts[i].replace(/^\s+/, '');
            if (p.indexOf(COOKIE + '=') === 0) return p.substring(COOKIE.length + 1);
        }
        return '';
    }
    function mutating(method) {
        var m = String(method || 'GET').toUpperCase();
        return m !== 'GET' && m !== 'HEAD' && m !== 'OPTIONS';
    }
    function sameOrigin(url) {
        try {
            return new URL(String(url), document.baseURI || w.location.href).origin === w.location.origin;
        } catch (e) {
            return false;
        }
    }
    function attr(el, name) {
        return el && el.getAttribute ? el.getAttribute(name) : null;
    }

    // htmx: every hx-post/put/patch/delete, form or element.
    document.addEventListener('htmx:configRequest', function (evt) {
        var d = evt && evt.detail;
        if (!d || !mutating(d.verb) || !sameOrigin(d.path)) return;
        var t = token();
        if (t) d.headers[HEADER] = t;
    });

    // fetch(): the console's own scripts.
    var origFetch = w.fetch;
    if (typeof origFetch === 'function') {
        w.fetch = function (input, init) {
            try {
                var isReq = typeof Request !== 'undefined' && input instanceof Request;
                var method = (init && init.method) || (isReq ? input.method : 'GET');
                var url = isReq ? input.url : input;
                var t = token();
                if (t && mutating(method) && sameOrigin(url)) {
                    var base = init && init.headers ? init.headers : (isReq ? input.headers : undefined);
                    var h = new Headers(base || {});
                    h.set(HEADER, t);
                    var next = Object.assign({}, init);
                    next.headers = h;
                    init = next;
                }
            } catch (e) {
                // Never break a request over the token; the server will
                // refuse it with a message that names the missing token.
            }
            return origFetch.call(this, input, init);
        };
    }

    // Plain forms: an HTML form navigation cannot set a header, so the token
    // travels as the FIRST field (the server peeks only at the first field).
    // getAttribute, not form.method/form.action: a child input named
    // "method" or "action" shadows those properties.
    function prepareForm(form, submitter) {
        if (!form || form.tagName !== 'FORM') return;
        var method = attr(submitter, 'formmethod') || attr(form, 'method') || 'get';
        var action = attr(submitter, 'formaction') || attr(form, 'action') || w.location.href;
        var existing = form.querySelector('input[name="' + FIELD + '"]');
        var t = token();
        if (String(method).toLowerCase() !== 'post' || !t || !sameOrigin(action)) {
            // A token must never reach a URL (GET) or another origin.
            if (existing && existing.parentNode) existing.parentNode.removeChild(existing);
            return;
        }
        if (!existing) {
            existing = document.createElement('input');
            existing.type = 'hidden';
            existing.name = FIELD;
        }
        existing.value = t; // refreshed every submit: follows cookie rotation
        // Load-bearing: the server reads ONLY the first field of the body.
        if (form.firstChild !== existing) form.insertBefore(existing, form.firstChild);
    }
    document.addEventListener('submit', function (evt) {
        try { prepareForm(evt.target, evt.submitter || null); } catch (e) { /* see fetch */ }
    }, true);

    // form.submit() fires no submit event.
    var F = w.HTMLFormElement && w.HTMLFormElement.prototype;
    if (F && typeof F.submit === 'function') {
        var origSubmit = F.submit;
        F.submit = function () {
            try { prepareForm(this, null); } catch (e) { /* see fetch */ }
            return origSubmit.apply(this, arguments);
        };
    }
})();
