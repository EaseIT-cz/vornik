// Vornik console: the one dispatcher of control actions (GitHub #416, phase 3
// of the CSP plan; web-ui-design.md, "C, phase 3: the plan in detail").
//
// A control names an action with data-on-<event>="<name>" (click, change,
// input, submit, keydown) and may pass JSON arguments in data-args. One
// bubble-phase listener per event type on document walks from the target up,
// running each named action as (element, event, ...args). The data-confirm
// listeners in pageHead are capture-phase, so a confirmation always runs
// first and a cancelled one runs no action.
//
// Actions come only from the registry: the built-ins below and what page
// scripts register with Vornik.action(name, fn) at top level. The registry is
// frozen at DOMContentLoaded; a later registration is refused. Nothing here
// looks a name up on window or evaluates an attribute, so markup can name
// only an action the page offers.
(function () {
    'use strict';
    var actions = Object.create(null);
    var frozen = false;

    function register(name, fn) {
        if (frozen || typeof name !== 'string' || typeof fn !== 'function' || actions[name]) {
            console.warn('Vornik.action: refused', name);
            return false;
        }
        actions[name] = fn;
        return true;
    }

    function byID(id) { return id ? document.getElementById(id) : null; }

    // The built-in actions, one per common handler shape.
    // The ⓘ hints (web-ui-design.md "Explanations behind an ⓘ — 2026-11-01"
    // and its amendment). The text stays in the hint's hidden .vk-hint-pop;
    // one body-level layer shows it, so a card's overflow or backdrop-filter
    // cannot clip it. Hover and focus show it; a tap pins it (is-open);
    // Escape, a click outside, scroll and resize hide it.
    var hintLayer = null, hintShown = null;
    function hintBtn(hint) { return hint.querySelector('.vk-hint-btn'); }
    function hideHint() {
        if (!hintShown) { return; }
        var b = hintBtn(hintShown);
        if (b) { b.removeAttribute('aria-describedby'); b.setAttribute('aria-expanded', 'false'); }
        hintShown.classList.remove('is-open');
        hintShown = null;
        if (hintLayer) { hintLayer.classList.remove('is-shown'); }
    }
    function showHint(hint) {
        var pop = hint.querySelector('.vk-hint-pop'), b = hintBtn(hint);
        if (!pop || !b) { return; }
        if (hintShown && hintShown !== hint) { hideHint(); }
        // A swap that replaced the body detaches the old layer: make a new one.
        if (!hintLayer || !hintLayer.isConnected) {
            hintLayer = document.createElement('div');
            hintLayer.id = 'vk-hint-layer';
            hintLayer.className = 'vk-hint-layer';
            hintLayer.setAttribute('role', 'tooltip');
            document.body.appendChild(hintLayer);
        }
        hintLayer.replaceChildren.apply(hintLayer, Array.prototype.map.call(pop.childNodes, function (n) { return n.cloneNode(true); }));
        hintLayer.classList.add('is-shown');
        var r = b.getBoundingClientRect(), w = hintLayer.offsetWidth, h = hintLayer.offsetHeight, m = 8;
        var left = Math.max(m, Math.min(r.left, window.innerWidth - w - m));
        var top = r.bottom + 4;
        if (top + h > window.innerHeight - m && r.top - h - 4 >= m) { top = r.top - h - 4; }
        hintLayer.style.left = left + 'px';
        hintLayer.style.top = Math.max(m, top) + 'px';
        b.setAttribute('aria-describedby', 'vk-hint-layer');
        hintShown = hint;
    }
    register('hint-toggle', function (el) {
        var hint = el.closest('.vk-hint');
        if (!hint) { return; }
        if (hint.classList.contains('is-open')) { hideHint(); return; }
        showHint(hint);
        hint.classList.add('is-open');
        el.setAttribute('aria-expanded', 'true');
    });
    function hintOf(t) { return t && t.closest ? t.closest('.vk-hint') : null; }
    document.addEventListener('mouseover', function (e) {
        var h = hintOf(e.target);
        if (h && !(hintShown && hintShown.classList.contains('is-open'))) { showHint(h); }
    });
    document.addEventListener('mouseout', function (e) {
        var h = hintOf(e.target);
        if (h && h === hintShown && !h.contains(e.relatedTarget) && !h.classList.contains('is-open')) { hideHint(); }
    });
    document.addEventListener('focusin', function (e) {
        var h = hintOf(e.target);
        if (h && !(hintShown && hintShown.classList.contains('is-open'))) { showHint(h); }
    });
    document.addEventListener('focusout', function (e) {
        var h = hintOf(e.target);
        if (h && h === hintShown && !h.classList.contains('is-open')) { hideHint(); }
    });
    document.addEventListener('click', function (e) {
        if (!hintOf(e.target)) { hideHint(); }
    });
    document.addEventListener('keydown', function (e) {
        if (e.key === 'Escape') { hideHint(); }
    });
    window.addEventListener('scroll', hideHint, true);
    window.addEventListener('resize', hideHint);

    register('submit-form', function (el) {
        // form.submit() fires no submit event, as this.form.submit() did; the
        // CSRF sender wraps HTMLFormElement.prototype.submit for this path.
        if (el.form) { el.form.submit(); }
    });
    register('stop', function (el, e) { e.stopPropagation(); });
    register('reload', function () { window.location.reload(); });
    register('navigate', function (el, e) {
        if (el.hasAttribute('data-stop')) { e.preventDefault(); e.stopPropagation(); }
        if (el.dataset.href) { window.location.href = el.dataset.href; }
    });
    // Always stops, unlike navigate: its one use (the projects setup link)
    // needs both, and an Enter that navigates must not also submit.
    register('navigate-on-enter', function (el, e) {
        if (e.key !== 'Enter') { return; }
        e.preventDefault();
        e.stopPropagation();
        if (el.dataset.href) { window.location.href = el.dataset.href; }
    });
    register('remove-closest', function (el) {
        var target = el.dataset.target && el.closest(el.dataset.target);
        if (target) { target.remove(); }
    });
    register('escape-clear', function (el, e) {
        if (e.key !== 'Escape') { return; }
        el.value = '';
        // One input event on the same element: its own filter action runs.
        // It cannot recurse: this action listens to keydown.
        el.dispatchEvent(new Event('input', { bubbles: true }));
    });
    register('check-all', function (el) {
        var name = el.dataset.name;
        if (!name) { return; }
        document.querySelectorAll('input[name="' + CSS.escape(name) + '"]').forEach(function (c) { c.checked = el.checked; });
    });
    register('set-form-action', function (el) {
        var form = byID(el.dataset.form);
        var url = el.checked ? el.dataset.actionOn : el.dataset.actionOff;
        if (form && url) { form.action = url; }
    });
    register('show-modal', function (el) {
        var d = byID(el.dataset.dialog);
        if (d && d.showModal) { d.showModal(); }
    });
    register('close-modal', function (el) {
        var d = byID(el.dataset.dialog);
        if (d && d.close) { d.close(); }
    });
    // A filter box over a long list (#748, web-ui-design.md "Progressive
    // disclosure", B3): hides the [data-filter-item] rows of the nearest
    // [data-filter-scope] whose name does not contain the text. It only hides:
    // a hidden checkbox keeps its state and still posts.
    register('filter-list', function (el, e) {
        if (e.type === 'keydown') { return e.key !== 'Enter'; }
        var scope = el.closest('[data-filter-scope]');
        if (!scope) { return; }
        var q = el.value.trim().toLowerCase();
        scope.querySelectorAll('[data-filter-item]').forEach(function (item) {
            var name = (item.getAttribute('data-filter-item') || '').toLowerCase();
            // style, not the hidden attribute: a row's display utility (flex) outranks [hidden].
            item.style.display = q !== '' && name.indexOf(q) === -1 ? 'none' : '';
        });
    });

    function args(el) {
        var raw = el.getAttribute('data-args');
        if (!raw) { return []; }
        try {
            var v = JSON.parse(raw);
            return Array.isArray(v) ? v : [v];
        } catch (err) {
            console.warn('Vornik: data-args is not JSON on', el);
            return null;
        }
    }

    function dispatch(e) {
        var attr = 'data-on-' + e.type;
        var stopped = false;
        var origStop = e.stopPropagation;
        e.stopPropagation = function () { stopped = true; origStop.call(e); };
        for (var el = e.target; el && el !== document && !stopped; el = el.parentElement) {
            if (!el.getAttribute) { continue; }
            var name = el.getAttribute(attr);
            if (!name) { continue; }
            var fn = actions[name];
            if (!fn) {
                console.warn('Vornik: no action', name);
                continue;
            }
            var a = args(el);
            if (a === null) {
                // A malformed data-args runs nothing and cancels what it can,
                // so a guard cannot silently stop guarding (review 8beb N6).
                if (e.cancelable) { e.preventDefault(); }
                continue;
            }
            if (fn.apply(null, [el, e].concat(a)) === false) { e.preventDefault(); }
        }
    }

    // Phase 4 (#416): a timer poll on a data-poll element stops once the page
    // has poll-stopped (pageHead sets it on HX-Trigger: stopPolling). A timer
    // poll carries no triggering event; an event trigger on the same element
    // (sse:status) does, and is not cancelled. This replaces an hx-trigger
    // filter, which htmx would evaluate as script.
    document.addEventListener('htmx:confirm', function (e) {
        var el = e.target;
        if (el && el.hasAttribute && el.hasAttribute('data-poll') && !(e.detail && e.detail.triggeringEvent) &&
            document.body.classList.contains('poll-stopped')) {
            e.preventDefault();
        }
    });

    ['click', 'change', 'input', 'submit', 'keydown'].forEach(function (type) {
        document.addEventListener(type, dispatch);
    });
    // Frozen at DOMContentLoaded, or at once when this script runs after the
    // document has been parsed (review 8beb N5): late registration is refused
    // however console.js was loaded.
    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', function () { frozen = true; });
    } else {
        frozen = true;
    }

    // names lists the registered actions (read-only; the tests and a
    // debugging operator read it).
    window.Vornik = Object.freeze({ action: register, names: function () { return Object.keys(actions); } });
})();

// Autonomy next-eval countdown (web UI design, "The overview answers...", §D):
// every .autonomy-eta-label[data-next-eval-iso] on a page (the overview and
// Projects) ticks once a second while the tab is visible, and reads "due now"
// once its deadline passes. Shared here so neither page needs an inline script.
(function () {
    function fmt(deltaSec) {
        if (deltaSec <= 0) return "due now";
        if (deltaSec < 60) return "in " + deltaSec + "s";
        var m = Math.floor(deltaSec / 60), s = deltaSec % 60;
        if (m < 60) return "in " + m + "m " + (s < 10 ? "0" : "") + s + "s";
        var h = Math.floor(m / 60), mm = m % 60;
        return "in " + h + "h " + (mm < 10 ? "0" : "") + mm + "m";
    }
    function tick() {
        if (document.hidden) return;
        var now = Date.now();
        document.querySelectorAll(".autonomy-eta-label[data-next-eval-iso]").forEach(function (el) {
            var when = Date.parse(el.getAttribute("data-next-eval-iso") || "");
            if (isNaN(when)) return;
            el.textContent = fmt(Math.floor((when - now) / 1000));
        });
    }
    // Once when the page is ready, then every second.
    if (document.readyState === "loading") {
        document.addEventListener("DOMContentLoaded", tick);
    } else {
        tick();
    }
    setInterval(tick, 1000);
    document.addEventListener("visibilitychange", function () {
        if (!document.hidden) tick();
    });
})();

// Disclosures and what they hide (#748, web-ui-design.md "Progressive
// disclosure across the console", B1). A field inside a closed <details> still
// posts, but the browser cannot focus it to report it invalid, and an anchor
// to a section inside one lands on nothing visible. Both open the <details>
// first. Both listeners act on every page console.js loads, not only the
// settings form: any closed <details> around an invalid field or an anchor
// target opens (review-20261113-700d).
(function () {
    function openAncestors(el) {
        for (var d = el && el.closest ? el.closest('details') : null; d; d = d.parentElement && d.parentElement.closest('details')) {
            d.open = true;
        }
    }
    document.addEventListener('invalid', function (e) { openAncestors(e.target); }, true);
    function openHash() {
        var id;
        try { id = decodeURIComponent((location.hash || '').slice(1)); } catch (err) { return; }
        var el = id ? document.getElementById(id) : null;
        if (!el) return;
        // The browser then scrolls to the fragment itself (measured in
        // Chromium: the scroll lands after the section opens).
        openAncestors(el);
    }
    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', openHash);
    } else {
        openHash();
    }
    window.addEventListener('hashchange', openHash);
})();
