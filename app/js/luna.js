/* Talks to the root daemon through the Homebrew Channel exec service.
 *
 * Every request becomes:   <app dir>/bin/norrsken ctl <base64 JSON>
 * run as root by luna://org.webosbrew.hbchannel.service/exec. The binary
 * forwards it to the long-running daemon (starting it if needed) and prints
 * one JSON line: {"ok":true,"data":...} or {"ok":false,"error":"..."}.
 */
(function () {
  'use strict';

  var HB_EXEC = 'luna://org.webosbrew.hbchannel.service/exec';
  var live = []; // keep bridges referenced until they answer

  function lunaCall(uri, params, timeoutMs) {
    return new Promise(function (resolve, reject) {
      var bridge;
      try {
        bridge = new PalmServiceBridge();
      } catch (e) {
        reject(new Error('NO_BRIDGE'));
        return;
      }
      live.push(bridge);
      var done = false;
      function finish() {
        done = true;
        var i = live.indexOf(bridge);
        if (i >= 0) live.splice(i, 1);
      }
      var timer = setTimeout(function () {
        if (done) return;
        finish();
        try { bridge.cancel(); } catch (e) { /* ignore */ }
        reject(new Error('The TV did not answer in time.'));
      }, timeoutMs || 30000);
      bridge.onservicecallback = function (msg) {
        if (done) return;
        clearTimeout(timer);
        finish();
        var res;
        try { res = JSON.parse(msg); } catch (e) { reject(new Error('Unreadable reply from the TV.')); return; }
        resolve(res);
      };
      bridge.call(uri, JSON.stringify(params || {}));
    });
  }

  function appDir() {
    var p = decodeURIComponent(window.location.pathname || '');
    return p.replace(/\/[^\/]*$/, '');
  }

  function shellQuote(s) {
    return "'" + String(s).replace(/'/g, "'\\''") + "'";
  }

  function toBase64(str) {
    return window.btoa(unescape(encodeURIComponent(str)));
  }

  function DaemonError(message, code) {
    var e = new Error(message);
    e.code = code || 'DAEMON';
    return e;
  }

  var BIN = appDir() + '/bin/norrsken';

  function request(cmd, args, timeoutMs) {
    var payload = { cmd: cmd };
    if (args) for (var k in args) if (Object.prototype.hasOwnProperty.call(args, k)) payload[k] = args[k];
    var command = 'B=' + shellQuote(BIN) + '; [ -x "$B" ] || chmod 755 "$B"; exec "$B" ctl ' + toBase64(JSON.stringify(payload));
    return lunaCall(HB_EXEC, { command: command }, timeoutMs || 50000).then(function (res) {
      var out = String(res.stdoutString || '').trim();
      if (!out) {
        var why = res.errorText || res.stderrString || 'No reply from the Norrsken service.';
        var code = /does not exist|Unknown method|not found/i.test(why) ? 'NO_HBCHANNEL'
          : /denied|permission|not allowed/i.test(why) ? 'DENIED' : 'EXEC';
        throw DaemonError(why, code);
      }
      var line = out.split('\n').pop();
      var r;
      try { r = JSON.parse(line); } catch (e) { throw DaemonError('Unexpected reply: ' + line.slice(0, 160), 'EXEC'); }
      if (!r.ok) throw DaemonError(r.error || 'Request failed.');
      return r.data;
    });
  }

  window.Daemon = {
    available: typeof window.PalmServiceBridge !== 'undefined',
    binary: BIN,
    request: request
  };
})();
