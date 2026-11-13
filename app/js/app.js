(function () {
  'use strict';

  var $ = function (id) { return document.getElementById(id); };
  var stage = $('stage');
  var demo = !window.Daemon.available;
  var api = demo ? window.MockDaemon.request : window.Daemon.request;

  var POPULAR = ['US', 'GB', 'SE', 'DE', 'NL', 'CA', 'JP', 'FR', 'NO', 'AU'];
  var app = {
    status: null, countries: null, screen: null, pollTimer: null, pollFails: 0,
    linking: false, linkCanCancel: false, pairExpires: 0, recentsKey: '', sheetOpen: false
  };

  // ---------- helpers ----------
  function el(tag, cls, text) { var e = document.createElement(tag); if (cls) e.className = cls; if (text != null) e.textContent = text; return e; }
  function setText(id, t) { var e = $(id); if (e && e.textContent !== t) e.textContent = t; }
  function hue(code) { var h = 0; for (var i = 0; i < code.length; i++) h = (h * 41 + code.charCodeAt(i) * 17) % 360; return h; }
  function badge(code, fast) {
    var b = el('span', 'badge' + (fast ? ' fast' : ''));
    if (fast) b.innerHTML = '<svg viewBox="0 0 24 24"><path d="M13 3 5 14h6l-1 7 8-11h-6z"/></svg>';
    else { b.textContent = code || '··'; b.style.setProperty('--h', hue(code || 'x')); }
    return b;
  }
  function bytes(n) {
    if (!n) return '0 B';
    var u = ['B', 'KB', 'MB', 'GB', 'TB'], i = 0;
    while (n >= 1000 && i < u.length - 1) { n /= 1000; i++; }
    return (n >= 100 || i === 0 ? n.toFixed(0) : n.toFixed(1)) + ' ' + u[i];
  }
  function duration(s) {
    s = Math.max(0, Math.floor(s));
    var h = Math.floor(s / 3600), m = Math.floor(s % 3600 / 60), sec = s % 60;
    var mm = (m < 10 ? '0' : '') + m, ss = (sec < 10 ? '0' : '') + sec;
    return h ? h + ':' + mm + ':' + ss : m + ':' + ss;
  }
  function sentence(s) { s = String(s || ''); if (!s) return s; s = s.charAt(0).toUpperCase() + s.slice(1); return /[.!?…]$/.test(s) ? s : s + '.'; }
  var toastTimer;
  function toast(msg, isErr) {
    var t = $('toast');
    t.textContent = msg; t.className = 'toast show' + (isErr ? ' err' : '');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { t.className = 'toast' + (isErr ? ' err' : ''); }, isErr ? 5200 : 3200);
  }
  function fit() {
    var s = Math.min(window.innerWidth / 1920, window.innerHeight / 1080);
    stage.style.transform = s === 1 ? '' : 'scale(' + s + ')';
  }

  // ---------- screens ----------
  var SCREENS = ['home', 'locations', 'settings', 'link', 'blocked'];
  function show(name) {
    if (app.screen === 'link' && name !== 'link') stopLinking();
    app.screen = name;
    SCREENS.forEach(function (s) { $('screen-' + s).hidden = s !== name; });
    var full = name === 'link' || name === 'blocked';
    stage.classList.toggle('has-full', full);
    var items = document.querySelectorAll('.rail-item');
    for (var i = 0; i < items.length; i++) items[i].classList.toggle('current', items[i].getAttribute('data-screen') === name);
    if (name === 'locations') renderLocations();
    var scr = $('screen-' + name);
    Nav.setScopes(full ? [scr] : [$('rail'), scr]);
    var main = { home: $('h-main'), blocked: $('b-retry') }[name];
    var cur = Nav.current();
    if (!cur || !scr.contains(cur)) {
      if (main) Nav.focus(main); else Nav.focusFirst();
    }
  }

  function showBlocked(title, body, detail) {
    setText('b-title', title);
    setText('b-body', body);
    setText('b-detail', detail || '');
    show('blocked');
  }

  function blockedFor(err) {
    var msg = (err && err.message) || String(err);
    if (err && err.code === 'NO_HBCHANNEL') {
      return showBlocked('Homebrew Channel is missing',
        'Norrsken runs its VPN engine through the Homebrew Channel root service. Install Homebrew Channel on a rooted TV, then open Norrsken again.', msg);
    }
    if (err && err.code === 'DENIED') {
      return showBlocked('Norrsken needs a rooted TV',
        'The Homebrew Channel service refused to run commands. That happens when the TV is in Developer Mode but not rooted.', msg);
    }
    showBlocked('Can\'t start the VPN service', 'Norrsken could not talk to its background service on this TV.', msg);
  }

  // ---------- status ----------
  function poll() {
    clearTimeout(app.pollTimer);
    if (document.hidden) { app.pollTimer = setTimeout(poll, 4000); return; }
    api('status').then(function (s) {
      app.pollFails = 0;
      render(s);
    }, function (err) {
      app.pollFails++;
      if (app.pollFails === 3) toast('Lost contact with the VPN service — retrying…', true);
      if (app.pollFails > 2 && err && err.code === 'NO_HBCHANNEL') blockedFor(err);
    }).then(function () {
      var s = app.status, fast = s && (s.state === 'connecting' || s.state === 'reconnecting');
      app.pollTimer = setTimeout(poll, fast || app.linking ? 1000 : 2000);
    });
  }

  function stateWord(s) {
    switch (s.state) {
      case 'connected': return 'Protected';
      case 'connecting': return 'Connecting';
      case 'reconnecting': return 'Reconnecting';
      case 'error': return 'Not protected';
      default: return 'Not protected';
    }
  }

  function render(s) {
    var prev = app.status;
    app.status = s;
    stage.className = 'st-' + (s.state || 'idle') + (stage.classList.contains('has-full') ? ' has-full' : '');
    setText('rail-status', stateWord(s));
    setText('h-state', stateWord(s));

    var busy = s.state === 'connecting' || s.state === 'reconnecting';
    var main = $('h-main');
    if (s.state === 'connected') { main.textContent = 'Disconnect'; main.classList.add('stop'); }
    else if (busy) { main.textContent = 'Cancel'; main.classList.add('stop'); }
    else { main.textContent = 'Quick connect'; main.classList.remove('stop'); }

    var srv = s.server;
    if (s.state === 'connected' && srv) {
      setText('h-title', srv.country || 'Connected');
      setText('h-sub', [srv.city, srv.name, srv.load != null ? 'Load ' + srv.load + '%' : ''].filter(Boolean).join('  ·  '));
    } else if (busy) {
      setText('h-title', (s.target && s.target.label) || (srv && srv.country) || 'Connecting');
      setText('h-sub', s.message || 'Connecting…');
    } else {
      setText('h-title', 'You\'re not connected');
      setText('h-sub', 'Apps on this TV use your real IP address.');
    }
    var msg = $('h-msg');
    var m = s.state === 'error' ? sentence(s.error) : (s.state === 'connected' ? '' : (busy ? '' : ''));
    if (s.state === 'connected' && s.network === false) m = 'The TV lost its network connection.';
    msg.textContent = m; msg.className = 'msg' + (s.state === 'error' ? ' err' : '');

    // stats
    var on = s.state === 'connected';
    var ins = s.insight;
    setText('s-ip', on ? (ins ? ins.ip : 'Checking…') : '—');
    setText('s-ip2', on ? (ins ? [ins.city, ins.country].filter(Boolean).join(', ') : (srv ? srv.ip : '')) : ' ');
    setText('s-time', on && s.connectedAt ? duration(s.now - s.connectedAt) : '—');
    setText('s-server', on && srv ? srv.hostname : ' ');
    setText('s-rx', on ? '↓ ' + bytes(s.rx) : '—');
    setText('s-tx', on ? '↑ ' + bytes(s.tx) : ' ');
    var tp = s.settings && s.settings.threatProtection;
    setText('s-dns', on ? (s.dns === 'none' ? 'TV default' : (tp ? 'Threat Protection' : 'NordVPN')) : '—');
    setText('s-dns2', on ? (s.dns === 'redirect' ? 'Every app covered' : s.dns === 'resolv' ? 'Via system resolver' : s.dns === 'none' ? 'Not overridden' : '') || ' ' : ' ');

    renderRecents(s);
    renderSettings(s);
    if (app.screen === 'locations' && (!prev || !prev.server !== !s.server || (prev.server && s.server && prev.server.countryId !== s.server.countryId))) markConnectedCard();

    // linking
    if (app.linking) {
      var done = (s.pairing && s.pairing.linked) || (!app.linkCanCancel && s.linked);
      if (done) {
        app.linking = false;
        toast('NordVPN account linked');
        show('home');
      }
    } else if (!s.linked && app.screen !== 'link' && app.screen !== 'blocked') {
      startLinking(false);
    }
  }

  function targetFromCountry(c) { return { kind: 'country', countryId: c.id, label: c.name, code: c.code }; }

  function renderRecents(s) {
    var list = (s.recents || []).filter(function (r) { return r.kind !== 'quick'; }).slice(0, 5);
    var key = JSON.stringify(list);
    if (key === app.recentsKey) return;
    app.recentsKey = key;
    var box = $('h-recents');
    var had = box.contains(document.activeElement);
    box.textContent = '';
    $('h-recents-wrap').style.visibility = list.length ? 'visible' : 'hidden';
    list.forEach(function (t) {
      var b = el('button', 'f chip');
      b.appendChild(badge(t.code || ''));
      b.appendChild(el('span', '', t.label || 'Location'));
      b.addEventListener('click', function () { connect(t); });
      box.appendChild(b);
    });
    if (had) Nav.focus(box.querySelector('.f') || $('h-main'));
  }

  function connect(t) {
    api('connect', { target: t }).then(render, function (e) { toast(sentence(e.message), true); });
    if (app.screen !== 'home') show('home');
  }

  // ---------- locations ----------
  function loadCountries(force) {
    return api('countries', force ? { refresh: true } : null).then(function (cs) {
      app.countries = cs;
    }, function () {
      app.countries = (window.NORRSKEN_FALLBACK_COUNTRIES || []).map(function (c) {
        return { id: c[0], name: c[1], code: c[2], serverCount: c[3], cities: c[4].map(function (x) { return { id: x[0], name: x[1], serverCount: x[2] }; }) };
      });
    }).then(function () { if (app.screen === 'locations') renderLocations(true); });
  }

  function countryCard(c) {
    var b = el('button', 'f card');
    b.setAttribute('data-country', c.id);
    b.appendChild(badge(c.code));
    var t = el('span', 'txt');
    t.appendChild(el('span', 'name', c.name));
    var cities = (c.cities || []).length;
    t.appendChild(el('span', 'meta', c.serverCount + ' servers' + (cities > 1 ? ' · ' + cities + ' cities' : '')));
    b.appendChild(t);
    b.addEventListener('click', function () { pickCountry(c); });
    return b;
  }

  var locRendered = false;
  function renderLocations(force) {
    if (!app.countries) { if (!force) loadCountries(); return; }
    if (locRendered && !force) { markConnectedCard(); return; }
    locRendered = true;
    var pop = $('loc-popular'), all = $('loc-all');
    var keep = Nav.current();
    pop.textContent = ''; all.textContent = '';
    var fast = el('button', 'f card');
    fast.appendChild(badge('', true));
    var ft = el('span', 'txt'); ft.appendChild(el('span', 'name', 'Fastest server')); ft.appendChild(el('span', 'meta', 'Nearest, least busy'));
    fast.appendChild(ft);
    fast.setAttribute('data-autofocus', '');
    fast.addEventListener('click', function () { connect({ kind: 'quick', label: 'Fastest server' }); });
    pop.appendChild(fast);
    var byCode = {};
    app.countries.forEach(function (c) { byCode[c.code] = c; });
    POPULAR.forEach(function (code) { if (byCode[code] && pop.children.length < 8) pop.appendChild(countryCard(byCode[code])); });
    app.countries.forEach(function (c) { all.appendChild(countryCard(c)); });
    setText('loc-count', '· ' + app.countries.length);
    markConnectedCard();
    if (app.screen === 'locations' && (!keep || !document.body.contains(keep) || !$('screen-locations').contains(keep))) Nav.focus(fast);
  }

  function markConnectedCard() {
    var id = app.status && app.status.state === 'connected' && app.status.server ? app.status.server.countryId : -1;
    var cards = document.querySelectorAll('#screen-locations .card[data-country]');
    for (var i = 0; i < cards.length; i++) cards[i].classList.toggle('on', +cards[i].getAttribute('data-country') === id);
  }

  function pickCountry(c) {
    var cities = c.cities || [];
    if (cities.length <= 1) { connect(targetFromCountry(c)); return; }
    var grid = el('div', 'grid');
    var best = el('button', 'f card');
    best.appendChild(badge('', true));
    var bt = el('span', 'txt'); bt.appendChild(el('span', 'name', 'Fastest server')); bt.appendChild(el('span', 'meta', 'Any city · ' + c.serverCount + ' servers'));
    best.appendChild(bt);
    best.addEventListener('click', function () { closeSheet(); connect(targetFromCountry(c)); });
    grid.appendChild(best);
    cities.forEach(function (ci) {
      var b = el('button', 'f card');
      b.appendChild(badge(c.code));
      var t = el('span', 'txt'); t.appendChild(el('span', 'name', ci.name)); t.appendChild(el('span', 'meta', ci.serverCount + ' servers'));
      b.appendChild(t);
      b.addEventListener('click', function () { closeSheet(); connect({ kind: 'city', countryId: c.id, cityId: ci.id, label: ci.name, code: c.code }); });
      grid.appendChild(b);
    });
    var sc = el('div', 'scroller'); sc.appendChild(grid);
    openSheet(c.name, 'Choose a city, or let Norrsken pick the fastest server.', sc, best);
  }

  // ---------- settings ----------
  function renderSettings(s) {
    var rows = document.querySelectorAll('[data-setting]');
    for (var i = 0; i < rows.length; i++) {
      var k = rows[i].getAttribute('data-setting');
      rows[i].classList.toggle('on', !!(s.settings && s.settings[k]));
    }
    setText('acc-state', s.linked ? 'Linked — this TV has its own NordLynx key' : 'Not linked');
    $('acc-unlink').hidden = !s.linked;
    setText('about', 'Norrsken ' + (s.version || '') + ' · unofficial NordVPN client for rooted webOS. Not affiliated with Nord Security.' + (demo ? ' (demo mode)' : ''));
  }

  function toggleSetting(row) {
    var k = row.getAttribute('data-setting');
    var cur = !!(app.status && app.status.settings && app.status.settings[k]);
    var patch = {}; patch[k] = !cur;
    row.classList.add('busy');
    row.classList.toggle('on', !cur);
    api('setSettings', { settings: patch }).then(function (s) {
      render(s);
      if (k === 'killSwitch' && !cur) toast('Kill switch on — if the VPN drops, internet stays blocked until it reconnects');
    }, function (e) {
      row.classList.toggle('on', cur);
      toast(sentence(e.message), true);
    }).then(function () { row.classList.remove('busy'); });
  }

  // ---------- account linking ----------
  function startLinking(canCancel) {
    app.linking = true;
    app.linkCanCancel = !!canCancel;
    $('l-cancel').hidden = !canCancel;
    $('l-manual').hidden = true;
    $('l-msg').textContent = '';
    $('l-input').value = '';
    show('link');
    beginPairing();
  }

  function beginPairing() {
    setText('l-pin', '······'); setText('l-pin2', '······'); setText('l-url', '…'); setText('l-exp', ' ');
    api('pairStart').then(function (p) {
      if (!app.linking) return;
      setText('l-url', p.display.replace(/^http:\/\//, ''));
      setText('l-pin', p.pin.replace(/(\d{3})(\d{3})/, '$1 $2'));
      setText('l-pin2', p.pin);
      app.pairExpires = p.expires;
      drawQR(p.qr || []);
      tickExpiry();
    }, function (e) {
      $('l-msg').className = 'msg err';
      $('l-msg').textContent = 'Phone pairing is unavailable (' + sentence(e.message) + ') You can still type the token on the TV.';
      drawQR([]);
    });
  }

  var expTimer;
  function tickExpiry() {
    clearTimeout(expTimer);
    if (!app.linking) return;
    var left = app.pairExpires - Math.floor(Date.now() / 1000);
    if (left <= 0) { beginPairing(); return; }
    setText('l-exp', 'Code expires in ' + duration(left));
    expTimer = setTimeout(tickExpiry, 1000);
  }

  function stopLinking() {
    if (!app.linking && !expTimer) return;
    app.linking = false;
    clearTimeout(expTimer); expTimer = null;
    api('pairStop').then(null, function () {});
  }

  function drawQR(rows) {
    var c = $('l-qr'), g = c.getContext('2d');
    g.fillStyle = '#f6f8fc'; g.fillRect(0, 0, c.width, c.height);
    if (!rows.length) return;
    var n = rows.length, quiet = 2, cell = Math.floor(c.width / (n + quiet * 2)), off = Math.floor((c.width - cell * n) / 2);
    g.fillStyle = '#070a12';
    for (var y = 0; y < n; y++) for (var x = 0; x < n; x++) if (rows[y].charAt(x) === '1') g.fillRect(off + x * cell, off + y * cell, cell, cell);
  }

  function submitToken() {
    var v = $('l-input').value.trim();
    var m = $('l-msg');
    if (!v) { m.className = 'msg err'; m.textContent = 'Enter the access token first.'; return; }
    m.className = 'msg'; m.textContent = 'Checking with NordVPN…';
    api('setToken', { token: v }).then(function (s) {
      $('l-input').value = '';
      app.linking = false;
      render(s);
      toast('NordVPN account linked');
      show('home');
    }, function (e) { m.className = 'msg err'; m.textContent = sentence(e.message); });
  }

  // ---------- sheet ----------
  var sheetReturn = null;
  function openSheet(title, sub, body, focusEl) {
    sheetReturn = Nav.current();
    setText('sheet-title', title);
    setText('sheet-sub', sub || '');
    var b = $('sheet-body'); b.textContent = ''; b.appendChild(body);
    $('sheet').hidden = false;
    app.sheetOpen = true;
    var card = document.querySelector('.sheet-card');
    Nav.forget(card);
    Nav.setScopes([card]);
    Nav.focus(focusEl || card.querySelector('.f'));
  }
  function closeSheet() {
    if (!app.sheetOpen) return;
    app.sheetOpen = false;
    $('sheet').hidden = true;
    var full = app.screen === 'link' || app.screen === 'blocked';
    Nav.setScopes(full ? [$('screen-' + app.screen)] : [$('rail'), $('screen-' + app.screen)]);
    if (sheetReturn && document.body.contains(sheetReturn)) Nav.focus(sheetReturn);
  }

  function confirmSheet(title, sub, okLabel, onOk) {
    var a = el('div', 'actions');
    var ok = el('button', 'f btn primary', okLabel);
    var no = el('button', 'f btn ghost', 'Cancel');
    ok.addEventListener('click', function () { closeSheet(); onOk(); });
    no.addEventListener('click', closeSheet);
    a.appendChild(no); a.appendChild(ok);
    openSheet(title, sub, a, no);
  }

  function showDiagnostics() {
    Promise.all([api('diag'), api('log', { lines: 40 }).then(null, function () { return []; })]).then(function (r) {
      var d = r[0], log = r[1] || [];
      var kv = el('div', 'kv');
      function add(k, v, bad) { kv.appendChild(el('span', 'k', k)); kv.appendChild(el('span', 'v' + (bad ? ' bad' : ''), v)); }
      add('Running as', d.uid === 0 ? 'root' : 'uid ' + d.uid + ' (needs root)', d.uid !== 0);
      add('webOS', [d.webos, d.firmware].filter(Boolean).join(' · ') || 'unknown');
      add('CPU / kernel', d.machine + ' · Linux ' + d.kernel);
      add('TUN device', d.tun ? 'available' : 'missing (Norrsken will try to create it)', !d.tun);
      add('DNS method', /\(|missing/.test(d.iptables) ? 'resolv.conf override (iptables NAT ' + (d.iptables === 'missing' ? 'not installed' : 'unsupported') + ')' : 'redirect all queries (' + d.iptables + ')');
      add('System DNS', (d.nameservers || []).join(', ') + (d.resolv ? '  (' + d.resolv + ')' : ''));
      add('Network', (d.uplink ? 'online' : 'no default route') + (d.lanIp ? ' · ' + d.lanIp : ''), !d.uplink);
      add('Start-up hook', d.autostart ? 'installed' : 'off');
      if (d.mountNs) add('Mount namespace', d.mountNs === 'separate' ? 'separate from the system (DNS override goes through nsenter)' : d.mountNs === 'init' ? 'shared with the system' : d.mountNs);
      var pre = el('pre', '', log.join('\n') || '(no log yet)');
      var sc = el('div', 'scroller');
      var inner = el('div', '');
      inner.appendChild(kv); inner.appendChild(pre);
      var close = el('button', 'f btn ghost', 'Close');
      close.addEventListener('click', closeSheet);
      var a = el('div', 'actions'); a.appendChild(close);
      sc.appendChild(inner);
      var wrap = el('div', ''); wrap.style.display = 'flex'; wrap.style.flexDirection = 'column'; wrap.style.minHeight = '0'; wrap.style.flex = '1';
      wrap.appendChild(sc); wrap.appendChild(a);
      openSheet('Diagnostics', 'Norrsken ' + d.version + ' (build ' + (d.build || '?') + ') · ' + d.binary, wrap, close);
    }, function (e) { toast(sentence(e.message), true); });
  }

  // ---------- wiring ----------
  function bind() {
    var items = document.querySelectorAll('.rail-item');
    for (var i = 0; i < items.length; i++) {
      items[i].addEventListener('click', function () { show(this.getAttribute('data-screen')); });
    }
    $('h-main').addEventListener('click', function () {
      var s = app.status || {};
      if (s.state === 'connected' || s.state === 'connecting' || s.state === 'reconnecting') {
        api('disconnect').then(render, function (e) { toast(sentence(e.message), true); });
      } else {
        connect({ kind: 'quick', label: 'Fastest server' });
      }
    });
    $('h-choose').addEventListener('click', function () { show('locations'); });
    var rows = document.querySelectorAll('[data-setting]');
    for (var j = 0; j < rows.length; j++) rows[j].addEventListener('click', function () { toggleSetting(this); });
    $('acc-relink').addEventListener('click', function () { startLinking(!!(app.status && app.status.linked)); });
    $('acc-unlink').addEventListener('click', function () {
      confirmSheet('Unlink this TV?', 'Norrsken disconnects and deletes the stored token and NordLynx key. You can link again any time.', 'Unlink', function () {
        api('logout').then(function (s) { render(s); }, function (e) { toast(sentence(e.message), true); });
      });
    });
    $('diag-open').addEventListener('click', showDiagnostics);
    $('l-type').addEventListener('click', function () {
      $('l-manual').hidden = false;
      Nav.focus($('l-input'));
    });
    $('l-cancel').addEventListener('click', function () { show('settings'); });
    $('l-submit').addEventListener('click', submitToken);
    $('b-retry').addEventListener('click', function () { start(); });

    Nav.onBack(function () {
      if (app.sheetOpen) { closeSheet(); return; }
      if (app.screen === 'link') {
        if (!$('l-manual').hidden) { $('l-manual').hidden = true; Nav.focus($('l-type')); return; }
        if (app.linkCanCancel) { show('settings'); return; }
        exitApp(); return;
      }
      if (app.screen === 'locations' || app.screen === 'settings') { show('home'); return; }
      exitApp();
    });
    Nav.onKey(function (k, target) {
      if (k === 13 && target && target.id === 'l-input') { submitToken(); return true; }
      if (k === 403 && app.status && app.status.linked) { $('h-main').click(); return true; } // red button
      return false;
    });
    document.addEventListener('visibilitychange', function () { if (!document.hidden) poll(); });
    window.addEventListener('resize', fit);
  }

  function exitApp() {
    if (window.PalmSystem && typeof window.PalmSystem.platformBack === 'function') window.PalmSystem.platformBack();
    else window.close();
  }

  function start() {
    api('status').then(function (s) {
      if (!s.root) {
        showBlocked('Norrsken needs a rooted TV',
          'Norrsken\'s VPN engine started, but without root rights it cannot create the tunnel. Root the TV and make sure Homebrew Channel shows "Root status: ok".', 'running as a normal user');
        return;
      }
      app.status = null;
      render(s);
      if (s.linked) show('home');
      poll();
      loadCountries();
    }, blockedFor);
  }

  fit();
  bind();
  if (demo) { var d = el('div', 'demo-badge', 'Demo — not on a TV'); stage.appendChild(d); }
  start();
})();
