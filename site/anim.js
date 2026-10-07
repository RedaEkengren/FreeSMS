/*
 * The scenes on the FreeSMS site.
 *
 * The engine Argusmetrics and BraLeads use on their sites, both Benbo's and
 * both AGPL like this project, brought over with one change: numbers are
 * written the Swedish way, with a space between the thousands. A scene is plain
 * HTML drawn on a fixed canvas (960x540, or 520x650 below 560px, class
 * an-m) and scaled to the width it is given, so scenes are laid out in px.
 * Elements are driven by attributes, in seconds from the start of the scene:
 *
 *   data-t="in,out"        visible between in and out (out omitted: to the end).
 *                          Entry: an-r (from the right), an-pop, an-fade.
 *   data-type="start,len"  types out the element's text
 *   data-count="start,len,from,to; start2,..."
 *                          counts evenly, several steps separated by ;
 *   .an-cursor data-path="0:480,600; 1.2:300,140c"
 *                          a pointer moving between points, c = click.
 *                          data-path-m is the same for the mobile layout.
 *
 * A scene runs only while at least 40% of it is on screen and the tab is
 * visible. With prefers-reduced-motion it draws one still frame, at
 * data-still seconds, and never moves.
 *
 * Without JavaScript every element is simply visible, which for every scene
 * here reads as the finished state.
 */
(function () {
  'use strict';
  if (window.__anEngine) return;
  window.__anEngine = true;

  var reduced = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;

  function nums(s) { return (s || '').split(',').map(parseFloat); }
  function fmt(n) { return Math.round(n).toLocaleString('sv-SE'); }
  function ease(x) { return x < .5 ? 2 * x * x : 1 - Math.pow(-2 * x + 2, 2) / 2; }

  function init(wrap) {
    var canvas = wrap.querySelector('.an-canvas');
    var loop = parseFloat(wrap.dataset.loop) || 15;
    var shown = [].slice.call(wrap.querySelectorAll('[data-t]')).map(function (el) {
      var t = nums(el.dataset.t);
      return { el: el, a: t[0], b: isNaN(t[1]) ? loop - .5 : t[1], on: false };
    });
    var typers = [].slice.call(wrap.querySelectorAll('[data-type]')).map(function (el) {
      var t = nums(el.dataset.type);
      return { el: el, a: t[0], d: t[1], text: el.textContent, n: -1 };
    });
    var counters = [].slice.call(wrap.querySelectorAll('[data-count]')).map(function (el) {
      return { el: el, segs: el.dataset.count.split(';').map(nums), v: null };
    });
    var cursor = wrap.querySelector('.an-cursor');
    var lastT = 0;

    function resize() {
      var w = wrap.clientWidth, mobile = w < 560;
      wrap.classList.toggle('an-m', mobile);
      canvas.style.transform = 'scale(' + (w / (mobile ? 520 : 960)) + ')';
    }

    function cursorPath() {
      var p = wrap.classList.contains('an-m') && cursor.dataset.pathM ? cursor.dataset.pathM : cursor.dataset.path;
      return p.split(';').map(function (s) {
        var m = s.trim().match(/^([\d.]+):(-?[\d.]+),(-?[\d.]+)(c?)$/);
        return { t: +m[1], x: +m[2], y: +m[3], click: !!m[4] };
      });
    }

    function render(t) {
      shown.forEach(function (s) {
        var on = t >= s.a && t < s.b;
        if (on !== s.on) { s.on = on; s.el.classList.toggle('an-on', on); }
      });
      typers.forEach(function (s) {
        var n = t < s.a ? 0 : Math.min(s.text.length, Math.floor((t - s.a) / s.d * s.text.length));
        if (n !== s.n) {
          s.n = n;
          s.el.textContent = s.text.slice(0, n);
          s.el.classList.toggle('an-caret', n > 0 && n < s.text.length);
        }
      });
      counters.forEach(function (s) {
        var v = s.segs[0][2];
        s.segs.forEach(function (g) {
          if (t > g[0]) v = g[2] + (g[3] - g[2]) * Math.min(1, (t - g[0]) / g[1]);
        });
        v = Math.round(v);
        if (v !== s.v) { s.v = v; s.el.textContent = fmt(v); }
      });
      if (cursor) {
        var path = cursorPath();
        var x = path[0].x, y = path[0].y;
        for (var i = 1; i < path.length; i++) {
          var p = path[i - 1], q = path[i];
          if (t >= q.t) { x = q.x; y = q.y; continue; }
          if (t > p.t) {
            // Glide during the last 0.7 s before each point, stand still otherwise.
            var start = Math.max(p.t, q.t - .7), k = t <= start ? 0 : ease((t - start) / (q.t - start));
            x = p.x + (q.x - p.x) * k; y = p.y + (q.y - p.y) * k;
          }
          break;
        }
        cursor.style.transform = 'translate(' + x + 'px,' + y + 'px)';
        path.forEach(function (q) {
          if (q.click && lastT < q.t && t >= q.t) {
            cursor.classList.remove('an-click'); void cursor.offsetWidth; cursor.classList.add('an-click');
          }
        });
      }
      lastT = t;
    }

    wrap.classList.add('an-js');
    // Draws the scene at a given second without playing it. Tests use it to
    // check a frame rather than wait for one, and so can anyone inspecting a
    // scene in a background tab, where requestAnimationFrame never fires.
    wrap.__anRender = render;
    resize();
    if (window.ResizeObserver) new ResizeObserver(resize).observe(wrap); else window.addEventListener('resize', resize);
    if (reduced) { render(parseFloat(wrap.dataset.still) || loop * .7); return; }

    var t = 0, prev = 0, raf = 0, running = false, visible = false;
    function tick(now) {
      if (!running) return;
      t += Math.min(.1, (now - prev) / 1000); prev = now;
      // A scene that has played to its end says so, so that a page can move
      // on to the next one (the tabs do, until somebody picks a tab).
      if (t >= loop) { t = 0; lastT = 0; wrap.dispatchEvent(new CustomEvent('an:loop')); }
      render(t);
      wrap.__anT = t;
      raf = requestAnimationFrame(tick);
    }
    function sync() {
      var go = visible && !document.hidden;
      if (go && !running) { running = true; prev = performance.now(); raf = requestAnimationFrame(tick); }
      else if (!go && running) { running = false; cancelAnimationFrame(raf); }
    }
    render(0);
    // From the beginning, for a scene whose tab was just opened.
    wrap.addEventListener('an:restart', function () { t = 0; lastT = 0; render(0); resize(); });
    document.addEventListener('visibilitychange', sync);
    if ('IntersectionObserver' in window) {
      new IntersectionObserver(function (es) { visible = es[0].isIntersecting; sync(); }, { threshold: 0.4 }).observe(wrap);
    } else { visible = true; sync(); }
  }

  function boot() { [].forEach.call(document.querySelectorAll('.an-wrap'), init); }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot); else boot();
})();
