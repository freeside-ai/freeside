// Networkless probe of a writer launch's init event (#2922): the production
// file_v7 agent line, run against the pinned CLI on the agent image with a
// mock API on loopback, so the image's own managed settings decide which
// built-in plugins load. It prints the launch's stdout, the transcript the
// conformance judge reads, as base64 on one JSON line; the Go test runs the
// judge on it, so this fixture asserts nothing about severance itself.
// ANTHROPIC_BASE_URL: https://code.claude.com/docs/en/env-vars
const fs = require('node:fs');
const http = require('node:http');
const { spawn, spawnSync } = require('node:child_process');
const assert = require('node:assert/strict');

async function main() {
  const config = JSON.parse(fs.readFileSync('/probe/config.json', 'utf8'));
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
  // Every inference request gets a one-word answer; the init event comes
  // before the first request, so the turn's outcome is only recorded.
  const server = http.createServer((req, res) => {
    req.resume();
    req.on('end', () => {
      if (req.url.split('?')[0] !== '/v1/messages') {
        res.writeHead(404);
        res.end('synthetic service unavailable');
        return;
      }
      res.writeHead(200, { 'Content-Type': 'text/event-stream' });
      const event = (type, data) => res.write(`event: ${type}\ndata: ${JSON.stringify({type, ...data})}\n\n`);
      event('message_start', {message: {id: 'msg_synthetic', type: 'message', role: 'assistant',
        model: 'synthetic', content: [], stop_reason: null, stop_sequence: null,
        usage: {input_tokens: 1, output_tokens: 0}}});
      event('content_block_start', {index: 0, content_block: {type: 'text', text: ''}});
      event('content_block_delta', {index: 0, delta: {type: 'text_delta', text: 'severance-probe-ok'}});
      event('content_block_stop', {index: 0});
      event('message_delta', {delta: {stop_reason: 'end_turn', stop_sequence: null}, usage: {output_tokens: 1}});
      event('message_stop', {});
      res.end();
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
    const chunks = [];
    child.stdout.on('data', chunk => chunks.push(chunk));
    child.stderr.resume();
    const timer = setTimeout(() => child.kill('SIGKILL'), 60000);
    const status = await new Promise((resolve, reject) => {
      child.on('error', reject);
      child.on('exit', (code, signal) => resolve(signal ?? code));
    });
    clearTimeout(timer);
    console.log(JSON.stringify({probe: 'init-severance', version: version.stdout.trim(), status,
      transcript: Buffer.concat(chunks).toString('base64')}));
  } finally { server.close(); }
}
main().catch(error => { console.error(error.message); process.exitCode = 1; });
