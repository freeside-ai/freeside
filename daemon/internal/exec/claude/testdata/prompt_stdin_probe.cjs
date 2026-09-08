// Networkless probe of the pinned CLI's actual user-message transport.
// ANTHROPIC_BASE_URL: https://code.claude.com/docs/en/env-vars
const fs = require('node:fs');
const http = require('node:http');
const crypto = require('node:crypto');
const { spawn, spawnSync } = require('node:child_process');
const assert = require('node:assert/strict');

async function main() {
  const config = JSON.parse(fs.readFileSync('/probe/config.json', 'utf8'));
  const prompt = fs.readFileSync('/probe/prompt.txt');
  const digest = bytes => crypto.createHash('sha256').update(bytes).digest('hex');
  assert.equal(digest(prompt), config.digest);
  fs.chmodSync('/probe', 0o755);
  fs.chmodSync('/probe/prompt.txt', 0o400);
  for (const path of ['/probe/home', '/probe/work']) {
    fs.mkdirSync(path);
    fs.chownSync(path, 1001, 1001);
  }
  fs.writeFileSync('/probe/instructions.txt', '');
  const drop = ['--reuid=1001', '--regid=1001', '--clear-groups',
    '--inh-caps=-all', '--ambient-caps=-all', '--bounding-set=-all', '--no-new-privs'];
  assert.notEqual(spawnSync('setpriv', [...drop, 'cat', '/probe/prompt.txt']).status, 0);
  const version = spawnSync('claude', ['--version'], { encoding: 'utf8' });
  assert.equal(version.status, 0);
  assert.match(version.stdout, /^2\.1\.220\b/);
  let matched = false;
  let requests = 0;
  const auxiliaryRoutes = new Set();
  let protocolError;
  const server = http.createServer((req, res) => {
    const chunks = [];
    req.on('data', chunk => chunks.push(chunk));
    req.on('end', () => {
      try {
        const route = req.url.split('?')[0];
        // The CLI also probes auxiliary services (for example /api/hello).
        // Keep those unavailable while proving the actual inference payload.
        if (route !== '/v1/messages') {
          auxiliaryRoutes.add(route);
          res.writeHead(404);
          res.end('synthetic service unavailable');
          return;
        }
        const body = JSON.parse(Buffer.concat(chunks));
        requests++;
        for (const message of body.messages) {
          if (message.role !== 'user') continue;
          const texts = typeof message.content === 'string' ? [message.content] :
            message.content.filter(part => part.type === 'text').map(part => part.text);
          if (texts.some(text => digest(Buffer.from(text)) === config.digest)) matched = true;
        }
        res.writeHead(200, { 'Content-Type': 'text/event-stream' });
        const event = (type, data) => res.write(`event: ${type}\ndata: ${JSON.stringify({type, ...data})}\n\n`);
        event('message_start', {message: {id: 'msg_synthetic', type: 'message', role: 'assistant',
          model: body.model, content: [], stop_reason: null, stop_sequence: null,
          usage: {input_tokens: 1, output_tokens: 0}}});
        event('content_block_start', {index: 0, content_block: {type: 'text', text: ''}});
        event('content_block_delta', {index: 0, delta: {type: 'text_delta', text: 'stdin-probe-ok'}});
        event('content_block_stop', {index: 0});
        event('message_delta', {delta: {stop_reason: 'end_turn', stop_sequence: null}, usage: {output_tokens: 1}});
        event('message_stop', {});
        res.end();
      } catch (error) {
        protocolError = error;
        res.writeHead(400);
        res.end('synthetic protocol mismatch');
      }
    });
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  try {
    const child = spawn('sh', ['-c', config.command], {cwd: '/probe/work', env: {
      PATH: process.env.PATH, HOME: '/probe/home', CLAUDE_CONFIG_DIR: '/probe/home',
      ANTHROPIC_BASE_URL: `http://127.0.0.1:${server.address().port}`,
      ANTHROPIC_API_KEY: 'synthetic-test-only', DISABLE_AUTOUPDATER: '1',
      DISABLE_TELEMETRY: '1', DISABLE_ERROR_REPORTING: '1',
      CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: '1', IS_SANDBOX: '1', API_TIMEOUT_MS: '30000'
    }});
    let output = '';
    child.stdout.on('data', chunk => { output += chunk; });
    child.stderr.resume();
    const status = await new Promise((resolve, reject) => {
      child.on('error', reject);
      child.on('exit', resolve);
    });
    assert.equal(status, 0, 'CLI exit');
    if (protocolError) throw protocolError;
    assert.ok(matched, 'complete prompt must occur as an exact user text block');
    assert.ok(output.split('\n').filter(Boolean).map(line => JSON.parse(line))
      .some(item => item.type === 'result' && !item.is_error && item.result === 'stdin-probe-ok'));
    console.log(JSON.stringify({passed: true, version: version.stdout.trim(),
      bytes: prompt.length, digest: config.digest, requests,
      auxiliaryRoutes: [...auxiliaryRoutes].sort()}));
  } finally { server.close(); }
}
main().catch(error => { console.error(error.message); process.exitCode = 1; });
