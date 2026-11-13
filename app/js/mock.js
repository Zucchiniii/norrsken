/* In-browser stand-in for the daemon, so the UI can be developed and
 * previewed off the TV. Used automatically when PalmServiceBridge is missing.
 * ?demo=unlinked | connected | blocked | norooted
 */
(function () {
  'use strict';
  var q = (window.location.search || '').replace(/^\?/, '').split('&').reduce(function (o, kv) {
    var p = kv.split('='); if (p[0]) o[decodeURIComponent(p[0])] = decodeURIComponent(p[1] || ''); return o;
  }, {});
  var mode = q.demo || '';

  function countries() {
    return (window.NORRSKEN_FALLBACK_COUNTRIES || []).map(function (c) {
      return { id: c[0], name: c[1], code: c[2], serverCount: c[3], cities: c[4].map(function (x) { return { id: x[0], name: x[1], serverCount: x[2] }; }) };
    });
  }

  var st = {
    version: '0.1.1', state: 'idle', linked: mode !== 'unlinked', root: mode !== 'norooted', network: true,
    settings: { autoConnect: true, killSwitch: false, threatProtection: false },
    recents: [
      { kind: 'country', countryId: 208, label: 'Sweden', code: 'SE' },
      { kind: 'country', countryId: 228, label: 'United States', code: 'US' },
      { kind: 'city', countryId: 227, cityId: 2989907, label: 'London', code: 'GB' }
    ],
    rx: 0, tx: 0, handshakeAgo: -1
  };
  var connectedAt = 0, timer = null, pair = null;

  function serverFor(t) {
    var all = countries();
    var c = all.filter(function (x) { return x.id === t.countryId; })[0] || all.filter(function (x) { return x.code === 'SE'; })[0];
    var city = (t.cityId && c.cities.filter(function (x) { return x.id === t.cityId; })[0]) || c.cities[0];
    var n = 100 + Math.floor(Math.random() * 400);
    return { hostname: c.code.toLowerCase() + n + '.nordvpn.com', name: c.name + ' #' + n, ip: '86.106.103.' + (n % 250), load: 3 + Math.floor(Math.random() * 30),
      country: c.name, countryCode: c.code, countryId: c.id, city: city ? city.name : '' };
  }

  function doConnect(t) {
    clearTimeout(timer);
    st.state = st.server ? 'reconnecting' : 'connecting';
    st.message = 'Finding the best server…';
    st.target = t; st.error = '';
    st.recents = [t].concat(st.recents.filter(function (r) { return r.label !== t.label; })).slice(0, 6);
    timer = setTimeout(function () {
      st.server = serverFor(t);
      st.message = 'Connecting to ' + st.server.name + '…';
      timer = setTimeout(function () {
        st.state = 'connected'; st.message = '';
        if (!connectedAt) connectedAt = Date.now();
        st.insight = { ip: st.server.ip, country: st.server.country, city: st.server.city, isp: 'Nord Security', protected: true };
      }, 1300);
    }, 900);
  }

  if (mode === 'connected') {
    st.server = serverFor({ countryId: 208 }); st.state = 'connected'; connectedAt = Date.now() - 3725000;
    st.target = st.recents[0];
    st.insight = { ip: st.server.ip, country: 'Sweden', city: 'Stockholm', isp: 'Nord Security', protected: true };
  }

  // Real QR code for the demo URL (generated with rsc.io/qr, like the daemon does).
  var DEMO_QR = ["11111110010101001001101111111","10000010110000111011101000001","10111010010111101011001011101","10111010000011110001101011101","10111010100000100001101011101","10000010011001000111001000001","11111110101010101010101111111","00000000011111101111000000000","10101010001100110111000010010","10110000011000110000001101001","00110111000101000100111110111","01101001001110010110011101010","00100011010100001111001101011","01001100100101011100001101101","01001011001000111110100000011","00011001101111101111010011010","01101111110010111110001000011","00000001111010110110011100101","10011111000111000000010001011","01010100001010000101111101010","10001110010000000110111110000","00000000101101000010100010111","11111110011010100011101011011","10000010000111110101100011010","10111010100010100110111111001","10111010001100100111110011011","10111010110011000111010011101","10000010000111010101110001010","11111110110001110110111001011"];

  function status() {
    var s = JSON.parse(JSON.stringify(st));
    s.now = Math.floor(Date.now() / 1000);
    if (st.state === 'connected') {
      var el = (Date.now() - connectedAt) / 1000;
      s.connectedAt = Math.floor(connectedAt / 1000); s.rx = Math.floor(el * 1850000); s.tx = Math.floor(el * 61000); s.handshakeAgo = Math.floor(el % 120);
      s.dns = st.settings.threatProtection ? 'redirect' : 'redirect';
    }
    if (pair) s.pairing = pair;
    return s;
  }

  function request(cmd, args) {
    args = args || {};
    return new Promise(function (resolve, reject) {
      setTimeout(function () {
        if (mode === 'blocked') { var e = new Error('Service does not exist: org.webosbrew.hbchannel.service.'); e.code = 'NO_HBCHANNEL'; reject(e); return; }
        switch (cmd) {
          case 'ping': return resolve({ version: st.version });
          case 'status': return resolve(status());
          case 'countries': return resolve(countries());
          case 'connect':
            if (!st.linked) return reject(new Error('link your NordVPN account first'));
            doConnect(args.target || { kind: 'quick', label: 'Fastest server' }); return resolve(status());
          case 'disconnect':
            clearTimeout(timer); st.state = 'idle'; st.server = null; st.insight = null; st.message = ''; connectedAt = 0; return resolve(status());
          case 'setToken':
            if (!args.token || args.token.replace(/\s/g, '').length < 20) return reject(new Error('that does not look like a NordVPN access token'));
            st.linked = true; if (pair) pair.linked = true; return resolve(status());
          case 'logout': st.linked = false; st.state = 'idle'; st.server = null; return resolve(status());
          case 'setSettings':
            for (var k in args.settings) st.settings[k] = args.settings[k];
            return resolve(status());
          case 'pairStart':
            pair = { url: 'http://192.168.1.50:8765/#482913', display: 'http://192.168.1.50:8765', pin: '482913', qr: DEMO_QR, expires: Math.floor(Date.now() / 1000) + 600, linked: false };
            if (q.pairlink) setTimeout(function () { st.linked = true; pair.linked = true; }, 6000);
            return resolve(pair);
          case 'pairStop': pair = null; return resolve(null);
          case 'log': return resolve(['11-13 02:10:01 norrsken 0.1.1 (build 20261113120000) starting (pid 4242, boot=false)', '11-13 02:10:03 using se434.nordvpn.com (Stockholm, Sweden, load 4%)', '11-13 02:10:04 connected to se434.nordvpn.com']);
          case 'diag': return resolve({ version: '0.1.1', build: '20261113120000', uid: 0, arch: 'arm', machine: 'armv7l', kernel: '5.4.268', webos: 'webOS TV 26', tun: true, iptables: '/usr/sbin/iptables', resolv: '/var/run/resolv.conf', nameservers: ['127.0.0.1'], uplink: true, lanIp: '192.168.1.50', autostart: true, mountNs: 'init', binary: '/media/developer/apps/usr/palm/applications/com.zucchiniii.norrsken/bin/norrsken', stateDir: '/var/lib/webosbrew/norrsken' });
        }
        reject(new Error('unknown command ' + cmd));
      }, cmd === 'status' ? 60 : 260);
    });
  }

  window.MockDaemon = { request: request, mode: mode };
})();
