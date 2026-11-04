// Test-only path 2 driver: one usage read under the setup token's bearer,
// outside the CLI. The root parent prepares and checks the auth sentinel like
// claude_usage_probe.cjs; the read runs as the dropped identity. Only status,
// a bounded body and failure flags reach the private capture: never the
// token, request headers or error text. Go reviews and sanitizes the capture.
const fs = require('node:fs');
const http = require('node:http');
const tls = require('node:tls');
const {spawn, spawnSync} = require('node:child_process');

// The headers the pinned CLI sends for GET /api/oauth/usage in the
// stream-json launch, as the test proxy observed them with a synthetic token:
// its first-party client's headers plus axios's default Accept. Its
// Accept-Encoding is left out because this driver does not decompress.
const authority = 'api.anthropic.com';
const route = '/api/oauth/usage';
const bodyLimit = 64 * 1024;

function child() {
  const proxy = new URL(process.env.PROBE_PROXY);
  const token = process.env.PROBE_TOKEN;
  delete process.env.PROBE_TOKEN;
  const result = {status: null, body: '', timedOut: false, transportFailed: false, overflow: false};
  let done = false;
  const finish = () => {
    if (done) return;
    done = true;
    process.stdout.write(JSON.stringify(result));
    process.exit(0);
  };
  setTimeout(() => { result.timedOut = true; finish(); }, 30000).unref();
  // Open the tunnel by hand so the read depends on no proxy environment support.
  const connect = http.request({host: proxy.hostname, port: proxy.port, method: 'CONNECT',
    path: authority + ':443', headers: {host: authority + ':443'}});
  connect.on('error', () => { result.transportFailed = true; finish(); });
  connect.on('connect', (response, socket) => {
    if (response.statusCode !== 200) { result.transportFailed = true; socket.destroy(); finish(); return; }
    const secure = tls.connect({socket, servername: authority, ca: fs.readFileSync(process.env.PROBE_CA)});
    secure.on('error', () => { result.transportFailed = true; finish(); });
    // Node uses createConnection only when no agent is set; `agent: false`
    // silently dials host:port instead. The inert loopback discard target makes
    // any such fallback fail locally, never carry the bearer elsewhere.
    const request = http.request({
      host: '127.0.0.1', port: 9, path: route, method: 'GET', createConnection: () => secure,
      headers: {
        Host: authority,
        Accept: 'application/json, text/plain, */*',
        'User-Agent': 'claude-cli/2.1.220 (external, sdk-cli)',
        Authorization: 'Bearer ' + token,
        'anthropic-beta': 'oauth-2025-04-20',
        'Content-Type': 'application/json',
      },
    }, upstream => {
      result.status = upstream.statusCode;
      let bytes = 0;
      upstream.setEncoding('utf8');
      upstream.on('data', chunk => {
        bytes += Buffer.byteLength(chunk);
        if (bytes > bodyLimit) { result.overflow = true; upstream.destroy(); finish(); return; }
        result.body += chunk;
      });
      upstream.on('end', finish);
      upstream.on('error', () => { result.transportFailed = true; finish(); });
    });
    request.on('error', () => { result.transportFailed = true; finish(); });
    request.end();
  });
  connect.end();
}

if (process.argv[2] === 'read') {
  child();
} else {
  const config = JSON.parse(fs.readFileSync('/probe/config.json', 'utf8'));
  const root = '/scratch';
  const token = fs.readFileSync('/probe/token', 'utf8').trim();
  const drop = ['--reuid=1001', '--regid=1001', '--clear-groups',
    '--inh-caps=-all', '--ambient-caps=-all', '--bounding-set=-all', '--no-new-privs'];
  const baseEnv = {PATH: process.env.PATH, HOME: root};
  const emit = value => fs.writeFileSync('/capture/result.json', JSON.stringify(value), {mode: 0o600});
  const authUnchanged = () => fs.readFileSync('/probe/token', 'utf8').trim() === token &&
    fs.readFileSync(root + '/config/.credentials.json', 'utf8') === '{}';
  const main = async () => {
    fs.mkdirSync('/capture', {mode: 0o700});
    fs.mkdirSync(root, {recursive: true});
    fs.chownSync(root, 0, 0); fs.chmodSync(root, 0o1777);
    fs.mkdirSync(root + '/config', {mode: 0o1777});
    fs.chmodSync(root + '/config', 0o1777);
    fs.writeFileSync(root + '/config/.credentials.json', '{}', {mode: 0o444});
    // Refute the post-read check before relying on it, as the CLI driver does.
    fs.writeFileSync(root + '/config/.credentials.json', '{"synthetic":true}');
    if (authUnchanged()) { emit({failure: 'auth_mutation_check_failed'}); return; }
    fs.writeFileSync(root + '/config/.credentials.json', '{}');
    if (!authUnchanged()) { emit({failure: 'auth_mutation_restore_failed'}); return; }
    for (const target of ['/probe/token', root + '/config/.credentials.json']) {
      if (spawnSync('setpriv', [...drop, 'sh', '-c', 'printf x >> "$1"', 'probe', target],
        {env: baseEnv, stdio: 'ignore', timeout: 5000}).status === 0 ||
          spawnSync('setpriv', [...drop, 'rm', '-f', target],
            {env: baseEnv, stdio: 'ignore', timeout: 5000}).status === 0) {
        emit({failure: 'auth_write_permitted'}); return;
      }
    }
    const version = spawnSync('claude', ['--version'], {env: baseEnv, encoding: 'utf8', timeout: 10000});
    if (version.status !== 0 || !/^2\.1\.220\b/.test(version.stdout)) {
      emit({failure: 'version_mismatch'}); return;
    }
    const reader = spawn('setpriv', [...drop, process.execPath, '/probe/read.cjs', 'read'], {
      cwd: root, env: {...baseEnv, PROBE_PROXY: config.proxy, PROBE_CA: '/probe/ca.pem', PROBE_TOKEN: token},
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let output = '', bytes = 0, overflow = false;
    reader.stderr.resume();
    reader.stdout.setEncoding('utf8');
    reader.stdout.on('data', chunk => {
      bytes += Buffer.byteLength(chunk);
      if (bytes > 2 * bodyLimit) { overflow = true; reader.kill('SIGKILL'); return; }
      output += chunk;
    });
    const timer = setTimeout(() => reader.kill('SIGKILL'), 60000);
    const status = await new Promise(resolve => {
      reader.on('error', () => resolve({code: null, signal: null, spawnFailed: true}));
      reader.on('close', (code, signal) => resolve({code, signal, spawnFailed: false}));
    });
    clearTimeout(timer);
    let read = {}, malformed = false;
    try { read = JSON.parse(output); } catch { malformed = true; }
    emit({mode: config.mode, version: '2.1.220', ...status, overflow: overflow || read.overflow === true,
      malformed, readStatus: read.status ?? null, readBody: read.body ?? '',
      timedOut: read.timedOut === true, transportFailed: read.transportFailed === true,
      authUnchanged: authUnchanged()});
  };
  main().catch(() => { emit({failure: 'driver_failed'}); process.exitCode = 1; });
}
