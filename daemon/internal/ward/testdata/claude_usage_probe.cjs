// Test-only driver. Raw stdout stays in a private capture, never container logs;
// stderr is drained and discarded. Go reviews and sanitizes the capture.
const fs = require('node:fs');
const {spawn, spawnSync} = require('node:child_process');
const config = JSON.parse(fs.readFileSync('/probe/config.json', 'utf8'));
const root = '/scratch';
const token = fs.readFileSync('/probe/token', 'utf8').trim();
const drop = ['--reuid=1001', '--regid=1001', '--clear-groups',
  '--inh-caps=-all', '--ambient-caps=-all', '--bounding-set=-all', '--no-new-privs'];
const baseEnv = {
  PATH: process.env.PATH, HOME: root, CLAUDE_CONFIG_DIR: root + '/config',
  DISABLE_AUTOUPDATER: '1', DISABLE_TELEMETRY: '1', DISABLE_ERROR_REPORTING: '1',
  CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: '1', IS_SANDBOX: '1',
  HTTPS_PROXY: config.proxy, HTTP_PROXY: config.proxy, NO_PROXY: '',
  NODE_EXTRA_CA_CERTS: '/probe/ca.pem', SSL_CERT_FILE: '/probe/ca.pem',
};
// Each variant is one bounded launch. Only `turn` and `turn_seeded` may infer.
// CLAUDE_CODE_OAUTH_SCOPES changes only the scopes the pinned CLI assumes for
// the setup token (default `user:inference`); it mints and stores nothing.
const variants = {
  idle: {duplex: true, turn: false, env: {}},
  idle_profile_scope: {duplex: true, turn: false, nonessential: true,
    env: {CLAUDE_CODE_OAUTH_SCOPES: 'user:inference user:profile'}},
  turn: {duplex: false, turn: true, env: {}},
  turn_seeded: {duplex: true, turn: true,
    env: {CLAUDE_CODE_OAUTH_SCOPES: 'user:inference user:profile'}},
};
const variant = variants[config.mode];
const common = ['-p', '--output-format', 'stream-json', '--verbose',
  '--dangerously-skip-permissions', '--safe-mode',
  '--append-system-prompt-file', '/probe/instructions.txt'];
function emit(value) {
  fs.writeFileSync('/capture/result.json', JSON.stringify(value), {mode: 0o600});
}
function authUnchanged() {
  return fs.readFileSync('/probe/token', 'utf8').trim() === token &&
    fs.readFileSync(root + '/config/.credentials.json', 'utf8') === '{}';
}
async function main() {
  fs.mkdirSync('/capture', {mode: 0o700});
  for (const dir of [root, root + '/work']) {
    fs.mkdirSync(dir, {recursive: true}); fs.chownSync(dir, 1001, 1001);
  }
  fs.chownSync(root, 0, 0); fs.chmodSync(root, 0o1777);
  // Root ownership and the sticky directory protect the auth sentinel while
  // permitting ordinary configuration writes by the dropped CLI identity.
  fs.mkdirSync(root + '/config', {mode: 0o1777});
  fs.chmodSync(root + '/config', 0o1777);
  fs.writeFileSync(root + '/config/.credentials.json', '{}', {mode: 0o444});
  // Refute the actual post-run check before relying on it for live evidence.
  fs.writeFileSync(root + '/config/.credentials.json', '{"synthetic":true}');
  if (authUnchanged()) { emit({failure: 'auth_mutation_check_failed'}); return; }
  fs.writeFileSync(root + '/config/.credentials.json', '{}');
  if (!authUnchanged()) { emit({failure: 'auth_mutation_restore_failed'}); return; }
  for (const target of ['/probe/token', root + '/config/.credentials.json']) {
    // Use the real dropped identity. Never read or echo the token. Success
    // invalidates the measurement before any provider invocation.
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
  if (!variant) { emit({failure: 'unknown_variant'}); return; }
  const args = [...common, '--session-id', 'e1c2d336-0584-43ca-936f-9c5065cb16bd'];
  if (variant.duplex) args.push('--input-format', 'stream-json');
  const env = {...baseEnv, ...variant.env, CLAUDE_CODE_OAUTH_TOKEN: token};
  // The pinned first-party client refuses its usage read under this switch.
  if (variant.nonessential) delete env.CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC;
  const child = spawn('setpriv', [...drop, 'claude', ...args], {
    cwd: root + '/work', env,
    stdio: ['pipe', 'pipe', 'pipe'], detached: true,
  });
  let output = '', pending = '', bytes = 0, timedOut = false, overflow = false;
  let initialized = false, answered = false, malformed = false, resulted = false;
  const prompt = 'Reply with exactly OK. Do not use tools.';
  const requests = [];
  const send = request => { requests.push(request); child.stdin.write(JSON.stringify(request) + '\n'); };
  const stop = () => { try { process.kill(-child.pid, 'SIGKILL'); } catch {} };
  const timer = setTimeout(() => { timedOut = true; stop(); }, 60000);
  child.stderr.resume();
  child.stdout.setEncoding('utf8');
  child.stdin.on('error', () => {});
  child.stdout.on('data', chunk => {
    bytes += Buffer.byteLength(chunk);
    if (bytes > 1024 * 1024) { overflow = true; stop(); return; }
    output += chunk.toString('utf8'); pending += chunk.toString('utf8');
    let end;
    while ((end = pending.indexOf('\n')) >= 0) {
      const line = pending.slice(0, end); pending = pending.slice(end + 1);
      if (!line.trim()) continue;
      let msg; try { msg = JSON.parse(line); } catch { malformed = true; continue; }
      if (!variant.duplex) continue;
      const readUsage = () => send({type: 'control_request', request_id: 'usage-read', request: {subtype: 'get_usage'}});
      // A seeded read waits for the one turn's result, so the turn's response
      // headers have reached the CLI before it answers.
      if (variant.turn && msg.type === 'result' && initialized && !resulted) { resulted = true; readUsage(); }
      if (msg.type !== 'control_response') continue;
      if (msg.response?.request_id === 'usage-init' && msg.response.subtype === 'success' && !initialized) {
        initialized = true;
        if (variant.turn) send({type: 'user', message: {role: 'user', content: prompt}});
        else readUsage();
      }
      if (msg.response?.request_id === 'usage-read') { answered = true; child.stdin.end(); }
    }
  });
  if (variant.duplex) {
    send({type: 'control_request', request_id: 'usage-init', request: {subtype: 'initialize'}});
  } else child.stdin.end(prompt + '\n');
  const status = await new Promise(resolve => {
    child.on('error', () => resolve({code: null, signal: null, spawnFailed: true}));
    child.on('close', (code, signal) => resolve({code, signal, spawnFailed: false}));
  });
  clearTimeout(timer);
  // No descendants survive a completed parent in this disposable probe.
  stop();
  emit({mode: config.mode, version: '2.1.220', args, requests, initialized, answered,
    timedOut, overflow, malformed, ...status, output,
    authUnchanged: authUnchanged()});
}
main().catch(() => { emit({failure: 'driver_failed'}); process.exitCode = 1; });
