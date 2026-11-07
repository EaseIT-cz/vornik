// Turns notifications on for this approver device (GitHub #163;
// agent-administered design part 09 §9.3a B.6). Loaded only by
// /ui/approve/push, the one approval page whose CSP allows a same-origin
// script. It registers the push worker, subscribes with the daemon's VAPID
// key (read from data-key) on the button press, as iOS requires, and posts
// the subscription to /ui/approve/push. The daemon checks it again.
(function () {
  'use strict';
  var box = document.getElementById('push');
  var on = document.getElementById('push-on');
  if (!box || !on) {
    return;
  }
  function show(id, text) {
    var el = document.getElementById(id);
    if (!el) {
      return;
    }
    if (text) {
      el.textContent = text;
    }
    el.hidden = false;
  }
  var ios = /iPad|iPhone|iPod/.test(navigator.userAgent) ||
    (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);
  var standalone = window.navigator.standalone === true ||
    (window.matchMedia && window.matchMedia('(display-mode: standalone)').matches);
  if (!('serviceWorker' in navigator) || !('PushManager' in window) || !('Notification' in window)) {
    on.hidden = true;
    show(ios && !standalone ? 'push-ios' : 'push-unsupported');
    return;
  }
  if (ios && !standalone) {
    show('push-ios');
  }
  function keyBytes(b64) {
    var pad = '='.repeat((4 - (b64.length % 4)) % 4);
    var raw = atob((b64 + pad).replace(/-/g, '+').replace(/_/g, '/'));
    var out = new Uint8Array(raw.length);
    for (var i = 0; i < raw.length; i++) {
      out[i] = raw.charCodeAt(i);
    }
    return out;
  }
  function active(reg) {
    if (reg.active) {
      return Promise.resolve(reg);
    }
    return new Promise(function (resolve) {
      var w = reg.installing || reg.waiting;
      w.addEventListener('statechange', function () {
        if (w.state === 'activated') {
          resolve(reg);
        }
      });
    });
  }
  on.addEventListener('click', function () {
    on.disabled = true;
    Notification.requestPermission().then(function (perm) {
      if (perm !== 'granted') {
        throw new Error('Notifications were not allowed for this page. Allow them in the browser settings and try again.');
      }
      return navigator.serviceWorker.register('/ui/static/approve-push-sw.js', { scope: '/ui/approve/push/' });
    }).then(active).then(function (reg) {
      return reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: keyBytes(box.dataset.key) });
    }).then(function (sub) {
      var j = sub.toJSON();
      var form = new URLSearchParams();
      form.set('endpoint', j.endpoint || '');
      form.set('p256dh', (j.keys && j.keys.p256dh) || '');
      form.set('auth', (j.keys && j.keys.auth) || '');
      return fetch('/ui/approve/push', { method: 'POST', body: form, credentials: 'same-origin', redirect: 'follow' });
    }).then(function (res) {
      if (!res.ok) {
        throw new Error(res.status === 400
          ? 'This browser\'s push service is not one Vornik sends to.'
          : 'Vornik could not save this device\'s notifications (' + res.status + ').');
      }
      window.location.assign('/ui/approve/push?done=on');
    }).catch(function (err) {
      on.disabled = false;
      show('push-error', err && err.message ? err.message : 'Notifications could not be turned on.');
    });
  });
}());
