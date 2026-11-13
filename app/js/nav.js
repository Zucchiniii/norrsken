/* Spatial navigation for the TV remote (and Magic Remote pointer).
 * Focusable elements carry class "f". Nav.setScopes([...roots]) limits
 * navigation to those containers; each root remembers its last focus so
 * moving back into it lands where you left off.
 */
(function () {
  'use strict';

  var KEY = { LEFT: 37, UP: 38, RIGHT: 39, DOWN: 40, ENTER: 13, BACK: 461, ESC: 27, BACKSPACE: 8, PGUP: 33, PGDN: 34 };
  var scopes = [document.body];
  var memory = [];
  var current = null;
  var handlers = { back: function () {}, key: function () { return false; } };

  function isVisible(el) {
    if (!el || el.disabled || el.hidden) return false;
    var r = el.getBoundingClientRect();
    if (r.width === 0 || r.height === 0) return false;
    for (var p = el; p && p !== document.body; p = p.parentElement) if (p.hidden) return false;
    return true;
  }

  function rootOf(el) {
    for (var i = 0; i < scopes.length; i++) if (scopes[i].contains(el)) return scopes[i];
    return null;
  }

  function candidates() {
    var out = [];
    scopes.forEach(function (root) {
      var list = root.querySelectorAll('.f');
      for (var i = 0; i < list.length; i++) if (isVisible(list[i])) out.push(list[i]);
    });
    return out;
  }

  function remember(el) {
    var root = rootOf(el);
    if (!root) return;
    for (var i = 0; i < memory.length; i++) if (memory[i][0] === root) { memory[i][1] = el; return; }
    memory.push([root, el]);
  }

  function recall(root) {
    for (var i = 0; i < memory.length; i++) if (memory[i][0] === root && isVisible(memory[i][1]) && root.contains(memory[i][1])) return memory[i][1];
    return null;
  }

  function ensureVisible(el) {
    var sc = el.closest('.scroller');
    if (!sc) return;
    var er = el.getBoundingClientRect(), sr = sc.getBoundingClientRect(), pad = 90;
    if (er.top < sr.top + pad) sc.scrollTop -= (sr.top + pad - er.top);
    else if (er.bottom > sr.bottom - pad) sc.scrollTop += (er.bottom - (sr.bottom - pad));
    if (sc.scrollTop < 140 && el === sc.querySelector('.f')) sc.scrollTop = 0;
  }

  function focus(el, opts) {
    if (!el) return;
    if (current && current !== el) current.classList.remove('is-focus');
    current = el;
    el.classList.add('is-focus');
    try { el.focus({ preventScroll: true }); } catch (e) { el.focus(); }
    if (!opts || !opts.noScroll) ensureVisible(el);
    remember(el);
  }

  function score(dir, a, b) {
    var ax = (a.left + a.right) / 2, ay = (a.top + a.bottom) / 2, bx = (b.left + b.right) / 2, by = (b.top + b.bottom) / 2;
    var primary, gap, center;
    if (dir === 'right') { if (bx <= ax + 1 || b.left < a.left + 1) return -1; primary = Math.max(0, b.left - a.right); gap = Math.max(0, Math.max(a.top, b.top) - Math.min(a.bottom, b.bottom)); center = Math.abs(by - ay); }
    else if (dir === 'left') { if (bx >= ax - 1 || b.right > a.right - 1) return -1; primary = Math.max(0, a.left - b.right); gap = Math.max(0, Math.max(a.top, b.top) - Math.min(a.bottom, b.bottom)); center = Math.abs(by - ay); }
    else if (dir === 'down') { if (by <= ay + 1 || b.top < a.top + 1) return -1; primary = Math.max(0, b.top - a.bottom); gap = Math.max(0, Math.max(a.left, b.left) - Math.min(a.right, b.right)); center = Math.abs(bx - ax); }
    else { if (by >= ay - 1 || b.bottom > a.bottom - 1) return -1; primary = Math.max(0, a.top - b.bottom); gap = Math.max(0, Math.max(a.left, b.left) - Math.min(a.right, b.right)); center = Math.abs(bx - ax); }
    return primary + gap * 4 + center * 0.15;
  }

  function move(dir) {
    var list = candidates();
    if (!current || list.indexOf(current) < 0) { focusFirst(); return; }
    var a = current.getBoundingClientRect(), best = null, bestScore = Infinity;
    for (var i = 0; i < list.length; i++) {
      if (list[i] === current) continue;
      var s = score(dir, a, list[i].getBoundingClientRect());
      if (s >= 0 && s < bestScore) { bestScore = s; best = list[i]; }
    }
    if (!best) return;
    var from = rootOf(current), to = rootOf(best);
    if (to && to !== from) best = recall(to) || best;
    focus(best);
  }

  function page(dirDown) {
    if (!current) return;
    var sc = current.closest('.scroller');
    if (!sc) return;
    var h = sc.clientHeight * 0.8, a = current.getBoundingClientRect();
    var target = dirDown ? a.top + h : a.top - h;
    var list = candidates().filter(function (el) { return sc.contains(el); });
    var best = null, d = Infinity;
    list.forEach(function (el) {
      var r = el.getBoundingClientRect();
      var dist = Math.abs(r.top - target) + Math.abs(r.left - a.left) * 0.3;
      if (dist < d) { d = dist; best = el; }
    });
    if (best) focus(best);
  }

  function focusFirst() {
    var last = scopes[scopes.length - 1];
    var r = recall(last);
    if (r) { focus(r); return; }
    var list = candidates();
    var inLast = list.filter(function (el) { return last && last.contains(el); });
    var pool = inLast.length ? inLast : list;
    var pref = pool.filter(function (el) { return el.hasAttribute('data-autofocus'); })[0];
    focus(pref || pool[0]);
  }

  document.addEventListener('keydown', function (e) {
    var k = e.keyCode;
    var inInput = document.activeElement && document.activeElement.tagName === 'INPUT';
    if (k === KEY.BACK || k === KEY.ESC || (k === KEY.BACKSPACE && !inInput)) { e.preventDefault(); handlers.back(); return; }
    if (inInput && (k === KEY.LEFT || k === KEY.RIGHT)) return; // move the caret
    if (k === KEY.LEFT) { e.preventDefault(); move('left'); return; }
    if (k === KEY.RIGHT) { e.preventDefault(); move('right'); return; }
    if (k === KEY.UP) { e.preventDefault(); move('up'); return; }
    if (k === KEY.DOWN) { e.preventDefault(); move('down'); return; }
    if (k === KEY.PGUP || k === KEY.PGDN) { e.preventDefault(); page(k === KEY.PGDN); return; }
    if (k === KEY.ENTER) {
      if (inInput) { if (handlers.key(k, document.activeElement)) e.preventDefault(); return; }
      if (current && document.activeElement === current) { e.preventDefault(); current.click(); }
      return;
    }
    if (handlers.key(k, current)) e.preventDefault();
  });

  document.addEventListener('mouseover', function (e) {
    var el = e.target.closest && e.target.closest('.f');
    if (el && el !== current && rootOf(el)) focus(el, { noScroll: true });
  });

  window.addEventListener('focusin', function (e) {
    if (e.target.classList && e.target.classList.contains('f') && e.target !== current) {
      if (current) current.classList.remove('is-focus');
      current = e.target; current.classList.add('is-focus'); remember(e.target);
    }
  });

  window.Nav = {
    setScopes: function (roots) { scopes = roots.filter(Boolean); if (!current || !rootOf(current) || !isVisible(current)) focusFirst(); },
    focus: focus,
    focusFirst: focusFirst,
    forget: function (root) { memory = memory.filter(function (m) { return m[0] !== root; }); },
    current: function () { return current; },
    onBack: function (fn) { handlers.back = fn; },
    onKey: function (fn) { handlers.key = fn; }
  };
})();
