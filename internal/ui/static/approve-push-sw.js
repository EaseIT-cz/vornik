// Approver push worker (GitHub #163; agent-administered design part 09
// §9.3a B.7). It shows a notification for each push and opens the approvals
// list when one is tapped. It has no fetch handler and its scope,
// /ui/approve/push/, holds no page: it never sees a page or a request,
// least of all one where a credential is typed.
//
// The payload is {"title": <subject>, "url": "/ui/approve/"}, encrypted to
// this browser by the daemon. The subject names an event, never a value.

const FALLBACK_TITLE = 'Vornik: open your approvals';
const BODY = 'Open Vornik approvals';
const HOME = '/ui/approve/';

// safeURL accepts only a same-origin path under the approvals list.
function safeURL(u) {
  if (typeof u !== 'string' || !u.startsWith(HOME) || u.startsWith('//')) {
    return HOME;
  }
  return u;
}

self.addEventListener('push', (event) => {
  let title = FALLBACK_TITLE;
  let url = HOME;
  try {
    const data = event.data ? event.data.json() : null;
    if (data && typeof data.title === 'string' && data.title.length > 0 && data.title.length <= 200) {
      title = data.title;
    }
    if (data) {
      url = safeURL(data.url);
    }
  } catch (e) {
    // An unreadable payload still shows the fallback, so a push is never silent.
  }
  event.waitUntil(self.registration.showNotification(title, {
    body: BODY,
    icon: '/ui/static/icon.svg',
    data: { url: url },
  }));
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const url = safeURL(event.notification.data && event.notification.data.url);
  event.waitUntil(self.clients.openWindow(url));
});
