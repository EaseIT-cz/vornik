// Vornik web chat: send without a page load, show the reply as it is being
// worked on, and keep working through a reload or a restart.
// Design: https://docs.vornik.io §2.2 to §2.5.
//
// The server renders every piece of markup that holds model text (the
// finished exchange arrives as HTML from html/template); this script only
// sets textContent for what it builds itself (the person's bubble, the
// status line, errors). The console's CSRF sender attaches its header to the
// same-origin fetch POSTs below.
(function () {
    'use strict';
    var root = document.querySelector('[data-chat]');
    if (!root || !window.fetch) return; // no script: the form posts as before
    var turnsURL = root.getAttribute('data-chat-turns');
    var form = root.querySelector('[data-chat-form]');
    var input = root.querySelector('[data-chat-input]');
    var send = root.querySelector('[data-chat-send]');
    var thread = root.querySelector('#chat-thread');
    if (!form || !input || !send || !thread || !turnsURL) return;
    var iconSend = send.querySelector('[data-chat-icon-send]');
    var iconStop = send.querySelector('[data-chat-icon-stop]');
    var running = null; // {id, pending, prompt, timer, misses}

    function el(tag, cls, text) {
        var e = document.createElement(tag);
        if (cls) e.className = cls;
        if (text !== undefined) e.textContent = text;
        return e;
    }
    function scrollDown() {
        window.scrollTo({ top: document.body.scrollHeight, behavior: 'smooth' });
    }
    function grow() {
        input.style.height = 'auto';
        input.style.height = Math.min(input.scrollHeight, 192) + 'px';
    }
    function setRunning(on) {
        send.setAttribute('aria-label', on ? 'Stop' : 'Send');
        if (iconSend) iconSend.classList.toggle('hidden', on);
        if (iconStop) iconStop.classList.toggle('hidden', !on);
        var reset = document.querySelector('[data-chat-new]');
        if (reset) reset.disabled = on;
    }
    function hideEmpty() {
        var empty = thread.querySelector('[data-chat-empty]');
        if (empty) empty.remove();
    }

    // The person's message and a "Thinking…" placeholder, shown at once.
    function pendingExchange(prompt) {
        var ex = el('div', 'chat-exchange space-y-3');
        ex.setAttribute('data-chat-pending', '');
        var row = el('div', 'flex justify-end');
        var bubble = el('div', 'chat-user', prompt);
        bubble.setAttribute('data-role', 'user');
        row.appendChild(bubble);
        var a = el('div', 'chat-assistant');
        a.setAttribute('data-role', 'pending');
        var thinking = el('div', 'chat-thinking');
        var dot = el('span', 'chat-thinking-dot');
        dot.setAttribute('aria-hidden', 'true');
        var status = el('span', '', 'Thinking…');
        status.setAttribute('data-chat-status', '');
        thinking.appendChild(dot);
        thinking.appendChild(document.createTextNode(' '));
        thinking.appendChild(status);
        a.appendChild(thinking);
        ex.appendChild(row);
        ex.appendChild(a);
        thread.appendChild(ex);
        return ex;
    }

    // endWith ends the turn on the page. html, when given, is the part of
    // the reply that was delivered before it stopped or failed (server
    // rendered and marked); it stays, with the message under it.
    function endWith(message, retryPrompt, html) {
        if (!running) return;
        var p = running.pending;
        var slot = p.querySelector('[data-role="pending"]');
        if (slot) {
            slot.textContent = '';
            if (html) {
                slot.insertAdjacentHTML('beforeend', html);
                enhance(slot);
            }
            if (message) slot.appendChild(el('p', 'chat-thinking', message));
            if (retryPrompt) {
                var retry = el('button', 'v-btn v-btn--secondary mt-2', 'Try again');
                retry.type = 'button';
                retry.addEventListener('click', function () {
                    p.remove();
                    start(retryPrompt);
                });
                slot.appendChild(retry);
            }
        }
        stopPolling();
    }
    function stopPolling() {
        if (running && running.timer) clearTimeout(running.timer);
        running = null;
        setRunning(false);
        send.disabled = false;
        input.focus();
    }

    function poll() {
        if (!running) return;
        var r = running;
        fetch(turnsURL + '/' + encodeURIComponent(r.id), { headers: { Accept: 'application/json' }, credentials: 'same-origin', cache: 'no-store' })
            .then(function (res) {
                if (res.status === 404) return { state: 'lost' };
                return res.json();
            })
            .then(function (b) {
                if (running !== r) return;
                r.misses = 0;
                var next = 1000;
                if (b.state === 'running') {
                    var s = r.pending.querySelector('[data-chat-status]');
                    if (s && b.status) s.textContent = b.status;
                    if (b.html && b.html !== r.html) {
                        // The reply so far, rendered on the server (§3.2).
                        var slot = r.pending.querySelector('[data-role="pending"]');
                        var live = slot && slot.querySelector('[data-chat-live]');
                        if (slot && !live) {
                            live = el('div');
                            live.setAttribute('data-chat-live', '');
                            slot.appendChild(live);
                        }
                        if (live) live.innerHTML = b.html;
                        r.html = b.html;
                        next = 300; // poll faster while the text grows
                        scrollDown();
                    }
                } else if (b.state === 'done') {
                    r.pending.insertAdjacentHTML('beforebegin', b.html || '');
                    var added = r.pending.previousElementSibling;
                    r.pending.remove();
                    enhance(thread);
                    stopPolling();
                    if (added) scrollDown();
                    return;
                } else if (b.state === 'failed') {
                    return endWith(b.html ? '' : (b.error || 'The reply failed.'), r.prompt, b.html);
                } else if (b.state === 'cancelled') {
                    return endWith(b.html ? 'Not saved: a reload will not show it.' : 'Stopped.', null, b.html);
                } else if (b.state === 'lost') {
                    if (!input.value) input.value = r.prompt;
                    grow();
                    return endWith('This reply was lost, probably in a restart. Send it again?');
                }
                r.timer = setTimeout(poll, next);
            })
            .catch(function () {
                if (running !== r) return;
                r.misses = (r.misses || 0) + 1;
                if (r.misses > 30) {
                    if (!input.value) input.value = r.prompt;
                    return endWith('Lost the connection to Vornik. Send it again when it is back.');
                }
                r.timer = setTimeout(poll, 2000);
            });
    }

    function start(prompt) {
        hideEmpty();
        var pending = pendingExchange(prompt);
        running = { id: '', pending: pending, prompt: prompt, misses: 0 };
        setRunning(true);
        scrollDown();
        var body = new URLSearchParams();
        body.set('prompt', prompt);
        fetch(turnsURL, {
            method: 'POST',
            headers: { Accept: 'application/json', 'Content-Type': 'application/x-www-form-urlencoded' },
            body: body.toString(),
            credentials: 'same-origin'
        }).then(function (res) {
            return res.json().catch(function () { return {}; }).then(function (b) { return { status: res.status, body: b }; });
        }).then(function (r) {
            if (!running || running.pending !== pending) return;
            if (r.status === 202 && r.body.turn) {
                running.id = r.body.turn;
                pending.querySelector('[data-role="pending"]').setAttribute('data-turn', r.body.turn);
                running.timer = setTimeout(poll, 600);
                return;
            }
            var msg = (r.body && r.body.error && r.body.error.message) || 'The message could not be sent.';
            if (!input.value) input.value = prompt;
            grow();
            endWith(msg);
        }).catch(function () {
            if (!input.value) input.value = prompt;
            grow();
            endWith('Could not reach Vornik. Send it again in a moment.');
        });
    }

    function stop() {
        if (!running || !running.id) return;
        send.disabled = true;
        fetch(turnsURL + '/' + encodeURIComponent(running.id) + '/cancel', {
            method: 'POST', headers: { Accept: 'application/json' }, credentials: 'same-origin'
        }).catch(function () {});
    }

    form.addEventListener('submit', function (e) {
        e.preventDefault();
        if (running) return stop();
        var prompt = input.value.trim();
        if (!prompt) return;
        input.value = '';
        grow();
        start(prompt);
    });

    // Enter sends and Shift+Enter breaks the line, where there is a keyboard
    // and a fine pointer; on a touch screen Enter is a newline and the
    // button sends.
    var finePointer = window.matchMedia && window.matchMedia('(pointer: fine)').matches;
    input.addEventListener('keydown', function (e) {
        if (finePointer && e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
            e.preventDefault();
            if (!running) form.requestSubmit ? form.requestSubmit() : form.dispatchEvent(new Event('submit', { cancelable: true }));
        }
    });
    input.addEventListener('input', grow);

    root.querySelectorAll('[data-chat-suggest]').forEach(function (b) {
        b.addEventListener('click', function () {
            input.value = b.textContent.trim();
            grow();
            input.focus();
        });
    });

    // Copy buttons: one under each reply, one on each code block.
    function copy(text, button) {
        if (!navigator.clipboard) return;
        navigator.clipboard.writeText(text).then(function () {
            var old = button.getAttribute('aria-label');
            button.setAttribute('aria-label', 'Copied');
            button.classList.add('text-accent-500');
            setTimeout(function () {
                button.setAttribute('aria-label', old || 'Copy');
                button.classList.remove('text-accent-500');
            }, 1500);
        });
    }
    function enhance(scope) {
        scope.querySelectorAll('[data-chat-copy]:not([data-bound])').forEach(function (b) {
            b.setAttribute('data-bound', '');
            b.addEventListener('click', function () {
                var md = b.closest('.chat-assistant').querySelector('.chat-md');
                if (md) copy(md.innerText, b);
            });
        });
        scope.querySelectorAll('.chat-md pre:not([data-bound])').forEach(function (pre) {
            pre.setAttribute('data-bound', '');
            var b = el('button', 'v-btn v-btn--quiet absolute right-1 top-1 text-xs', 'Copy');
            b.type = 'button';
            b.setAttribute('aria-label', 'Copy code');
            b.addEventListener('click', function () { var code = pre.querySelector('code'); copy((code || pre).innerText, b); });
            pre.appendChild(b);
        });
    }

    enhance(thread);
    grow();
    // A reload during a turn resumes polling it (§2.4).
    var resume = thread.querySelector('[data-chat-pending] [data-turn]');
    if (resume) {
        running = { id: resume.getAttribute('data-turn'), pending: resume.closest('[data-chat-pending]'), prompt: '', misses: 0 };
        var user = running.pending.querySelector('[data-role="user"]');
        if (user) running.prompt = user.textContent;
        setRunning(true);
        poll();
    }
    window.scrollTo(0, document.body.scrollHeight);
})();
