// GitHub #802: post the App manifest to GitHub as soon as the page loads.
// A static file because the console CSP admits no new inline script.
(function () {
  var f = document.getElementById('manifest-form');
  if (f) { f.submit(); }
})();
