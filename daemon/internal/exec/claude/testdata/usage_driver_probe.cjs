// Networkless probe of the file_v6 launch: the production usage driver in
// front of the pinned CLI, against a mock API on loopback. It proves the turn
// makes exactly one inference request, carries the prompt as one exact user
// text block, gets an answer to its one get_usage, and that the driver exits
// with the CLI's status once stdin closes. The mock sends rate-limit headers;
// whether the CLI reports them under an API key is recorded, not asserted.
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
  fs.chmodSync('/probe/usage-driver.cjs', 0o444);
  for (const path of ['/probe/home', '/probe/work']) {
    fs.mkdirSync(path);
    fs.chownSync(path, 1001, 1001);
  }
  fs.writeFileSync('/probe/instructions.txt', '');
  const version = spawnSync('claude', ['--version'], { encoding: 'utf8' });
  assert.equal(version.status, 0);
  assert.ok(version.stdout.startsWith(config.version + ' '), `CLI ${version.stdout.trim()}, want ${config.version}`);
  const reset = Math.floor(Date.now() / 1000) + 86400;
  let matched = 0;
  let requests = 0;
  const auxiliaryRoutes = new Set();
  let protocolError;
  const server = http.createServer((req, res) => {
    const chunks = [];
    req.on('data', chunk => chunks.push(chunk));
    req.on('end', () => {
      try {
        const route = req.url.split('?')[0];
        if (route !== '/v1/messages') {
          auxiliaryRoutes.add(req.method + ' ' + route);
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
          matched += texts.filter(text => digest(Buffer.from(text)) === config.digest).length;
        }
        res.writeHead(200, {
          'Content-Type': 'text/event-stream',
          'anthropic-ratelimit-unified-status': 'allowed',
          'anthropic-ratelimit-unified-7d-status': 'allowed',
          'anthropic-ratelimit-unified-7d-utilization': '0.25',
          'anthropic-ratelimit-unified-7d-reset': String(reset),
        });
        const event = (type, data) => res.write(`event: ${type}\ndata: ${JSON.stringify({type, ...data})}\n\n`);
        event('message_start', {message: {id: 'msg_synthetic', type: 'message', role: 'assistant',
          model: body.model, content: [], stop_reason: null, stop_sequence: null,
          usage: {input_tokens: 1, output_tokens: 0}}});
        event('content_block_start', {index: 0, content_block: {type: 'text', text: ''}});
        event('content_block_delta', {index: 0, delta: {type: 'text_delta', text: 'usage-probe-ok'}});
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
    const started = Date.now();
    const status = await new Promise((resolve, reject) => {
      child.on('error', reject);
      child.on('exit', resolve);
    });
    const seconds = (Date.now() - started) / 1000;
    if (protocolError) throw protocolError;
    assert.equal(status, 0, 'driver exit');
    assert.equal(requests, 1, 'exactly one inference request');
    assert.equal(matched, 1, 'the prompt must occur once, as an exact user text block');
    const records = output.split('\n').filter(Boolean).map(line => JSON.parse(line));
    const answer = id => records.filter(item => item.type === 'control_response' && item.response?.request_id === id);
    assert.equal(answer('usage-init').length, 1, 'one initialize answer');
    assert.equal(records.filter(item => item.type === 'result').length, 1, 'one result');
    assert.ok(records.some(item => item.type === 'result' && !item.is_error && item.result === 'usage-probe-ok'));
    const usage = answer('usage-read');
    assert.equal(usage.length, 1, 'one get_usage answer');
    // A usage answer within the driver's timeout shows stdin closed on the
    // answer, not on the 15-second fallback.
    assert.ok(seconds < 14, `driver ran ${seconds}s`);
    const limits = usage[0].response?.response?.rate_limits;
    console.log(JSON.stringify({passed: true, version: version.stdout.trim(),
      bytes: prompt.length, digest: config.digest, requests, seconds,
      usageSubtype: usage[0].response?.subtype,
      rateLimitWindows: limits && typeof limits === 'object' ? Object.keys(limits).sort() : null,
      auxiliaryRoutes: [...auxiliaryRoutes].sort()}));
  } finally { server.close(); }
}
main().catch(error => { console.error(error.message); process.exitCode = 1; });
