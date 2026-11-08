// Project page: install tabs opened on the visitor's system, copy
// buttons, the latest version from GitHub, and remembering the language
// picked. Without JavaScript the page still shows everything.
(function () {
  'use strict';
  var body = document.body;
  document.documentElement.classList.add('js');

  // Language: a choice made in the menu is remembered (see the redirect
  // in the English page's head).
  document.querySelectorAll('.langs a').forEach(function (a) {
    a.addEventListener('click', function () {
      try { localStorage.setItem('lang', a.getAttribute('hreflang')); } catch (e) {}
    });
  });

  // Install tabs.
  var tabs = Array.prototype.slice.call(document.querySelectorAll('[role="tab"]'));
  function show(tab, focus) {
    tabs.forEach(function (t) {
      var on = t === tab;
      t.setAttribute('aria-selected', on);
      t.tabIndex = on ? 0 : -1;
      document.getElementById(t.getAttribute('aria-controls')).hidden = !on;
    });
    if (focus) tab.focus();
  }
  if (tabs.length) {
    document.querySelector('.tabs').hidden = false;
    var ua = navigator.userAgent;
    var first = /Windows/.test(ua) ? 'tab-scoop'
      : /Mac OS X|Linux/.test(ua) && !/Android|iPhone|iPad/.test(ua) ? 'tab-brew'
      : 'tab-docker';
    show(document.getElementById(first));
    tabs.forEach(function (t, i) {
      t.addEventListener('click', function () { show(t); });
      t.addEventListener('keydown', function (e) {
        var d = e.key === 'ArrowRight' ? 1 : e.key === 'ArrowLeft' ? -1 : 0;
        if (d) show(tabs[(i + d + tabs.length) % tabs.length], true);
      });
    });
  }

  // Copy buttons.
  if (navigator.clipboard) {
    document.querySelectorAll('.cmd').forEach(function (box) {
      var b = document.createElement('button');
      b.className = 'copy';
      b.type = 'button';
      b.textContent = body.dataset.copy;
      b.addEventListener('click', function () {
        navigator.clipboard.writeText(box.querySelector('code').textContent).then(function () {
          b.textContent = body.dataset.copied;
          setTimeout(function () { b.textContent = body.dataset.copy; }, 1500);
        });
      });
      box.appendChild(b);
    });
  }

  // Latest version, from GitHub's public API.
  var ver = document.getElementById('version');
  if (ver && window.fetch) {
    fetch('https://api.github.com/repos/i234dev/MyMule/releases/latest')
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (rel) {
        if (!rel || !/^v[0-9][0-9A-Za-z.\-]*$/.test(rel.tag_name)) return;
        var a = ver.querySelector('a');
        a.textContent = rel.tag_name.slice(1);
        var d = new Date(rel.published_at);
        if (!isNaN(d)) {
          ver.querySelector('time').textContent =
            d.toLocaleDateString(document.documentElement.lang, { year: 'numeric', month: 'long', day: 'numeric' });
          ver.querySelector('time').dateTime = rel.published_at;
        }
        ver.hidden = false;
      })
      .catch(function () {});
  }
})();
