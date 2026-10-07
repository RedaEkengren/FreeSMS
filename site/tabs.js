/*
 * The scene tabs. Without JavaScript every scene is on the page, one after
 * another; with it, one at a time.
 *
 * A scene that plays to its end moves the tabs on to the next one, so a
 * visitor who just watches sees the whole product. Once somebody picks a tab
 * it stays where they put it.
 */
(function () {
  'use strict';

  function boot() {
    var sec = document.querySelector('.tabs-sec');
    if (!sec) return;
    var tabs = [].slice.call(sec.querySelectorAll('[role=tab]'));
    var panes = [].slice.call(sec.querySelectorAll('.pane'));
    if (!tabs.length || tabs.length !== panes.length) return;
    sec.classList.add('tabs-on');

    var current = -1, chosen = false;
    function show(i, focus) {
      current = i;
      tabs.forEach(function (t, k) {
        t.setAttribute('aria-selected', k === i ? 'true' : 'false');
        t.tabIndex = k === i ? 0 : -1;
      });
      panes.forEach(function (p, k) { p.hidden = k !== i; });
      if (focus) tabs[i].focus();
      var wrap = panes[i].querySelector('.an-wrap');
      if (wrap) wrap.dispatchEvent(new CustomEvent('an:restart'));
    }

    tabs.forEach(function (t, k) {
      panes[k].id = panes[k].id || 'scen-' + (k + 1);
      t.setAttribute('aria-controls', panes[k].id);
      t.addEventListener('click', function () { chosen = true; show(k); });
      t.addEventListener('keydown', function (e) {
        var step = e.key === 'ArrowRight' ? 1 : e.key === 'ArrowLeft' ? -1 : 0;
        if (!step) return;
        e.preventDefault();
        chosen = true;
        show((k + step + tabs.length) % tabs.length, true);
      });
      var wrap = panes[k].querySelector('.an-wrap');
      if (wrap) wrap.addEventListener('an:loop', function () {
        if (!chosen && k === current) show((k + 1) % tabs.length);
      });
    });
    show(0);
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot); else boot();
})();
