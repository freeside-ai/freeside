// Networkless probe of an operator input file read by the file_v6 launch: the
// production usage driver and CLI command, a root-owned read-only image under
// Ward's input target (outside the working directory), and a mock API on
// loopback. The mock's first answer asks the CLI's Read tool for the image;
// the probe passes only when the next request returns that image as an image
// block in the tool result, after privilege drop.
// ANTHROPIC_BASE_URL: https://code.claude.com/docs/en/env-vars
const fs = require('node:fs');
const http = require('node:http');
const crypto = require('node:crypto');
const { spawn, spawnSync } = require('node:child_process');
const assert = require('node:assert/strict');

async function main() {
  const config = JSON.parse(fs.readFileSync('/probe/config.json', 'utf8'));
  const prompt = fs.readFileSync('/probe/prompt.txt');
  const image = fs.readFileSync('/probe/image.png');
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
  // The tree Ward's seeder leaves: root-owned, directories 0755, files 0444.
  const target = config.inputPath;
  const dirs = [];
  for (let dir = target.slice(0, target.lastIndexOf('/')); dir !== '/var/lib'; dir = dir.slice(0, dir.lastIndexOf('/'))) {
    dirs.unshift(dir);
  }
  for (const dir of dirs) {
    fs.mkdirSync(dir, {recursive: true});
    fs.chmodSync(dir, 0o755);
  }
  fs.writeFileSync(target, image);
  fs.chmodSync(target, 0o444);
  const version = spawnSync('claude', ['--version'], { encoding: 'utf8' });
  assert.equal(version.status, 0);
  assert.ok(version.stdout.startsWith(config.version + ' '), `CLI ${version.stdout.trim()}, want ${config.version}`);
  const toolUseID = 'toolu_synthetic_input_read';
  let requests = 0;
  let imageBlock;
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
          if (message.role !== 'user' || typeof message.content === 'string') continue;
          for (const part of message.content) {
            if (part.type !== 'tool_result' || part.tool_use_id !== toolUseID) continue;
            const content = Array.isArray(part.content) ? part.content : [];
            imageBlock = content.find(block => block.type === 'image') || imageBlock;
          }
        }
        res.writeHead(200, { 'Content-Type': 'text/event-stream' });
        const event = (type, data) => res.write(`event: ${type}\ndata: ${JSON.stringify({type, ...data})}\n\n`);
        event('message_start', {message: {id: 'msg_synthetic_' + requests, type: 'message', role: 'assistant',
          model: body.model, content: [], stop_reason: null, stop_sequence: null,
          usage: {input_tokens: 1, output_tokens: 0}}});
        if (requests === 1) {
          event('content_block_start', {index: 0, content_block: {type: 'tool_use', id: toolUseID, name: 'Read', input: {}}});
          event('content_block_delta', {index: 0, delta: {type: 'input_json_delta',
            partial_json: JSON.stringify({file_path: target})}});
          event('content_block_stop', {index: 0});
          event('message_delta', {delta: {stop_reason: 'tool_use', stop_sequence: null}, usage: {output_tokens: 1}});
        } else {
          event('content_block_start', {index: 0, content_block: {type: 'text', text: ''}});
          event('content_block_delta', {index: 0, delta: {type: 'text_delta', text: 'input-read-ok'}});
          event('content_block_stop', {index: 0});
          event('message_delta', {delta: {stop_reason: 'end_turn', stop_sequence: null}, usage: {output_tokens: 1}});
        }
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
    if (protocolError) throw protocolError;
    assert.ok(imageBlock, 'the Read result must carry an image block');
    assert.equal(imageBlock.source?.type, 'base64');
    assert.equal(imageBlock.source?.media_type, 'image/png');
    // The specifier's command also passes an output schema the mock's plain
    // answer does not meet; there only the read itself is asserted.
    if (!config.specification) {
      assert.equal(status, 0, 'driver exit');
      assert.equal(requests, 2, 'one tool turn and one answer');
      const records = output.split('\n').filter(Boolean).map(line => JSON.parse(line));
      assert.ok(records.some(item => item.type === 'result' && !item.is_error && item.result === 'input-read-ok'));
    }
    const returned = Buffer.from(imageBlock.source.data, 'base64');
    console.log(JSON.stringify({passed: true, specification: config.specification, status, version: version.stdout.trim(), requests,
      path: target, imageBytes: image.length, returnedBytes: returned.length,
      identical: digest(returned) === digest(image),
      auxiliaryRoutes: [...auxiliaryRoutes].sort()}));
  } finally { server.close(); }
}
main().catch(error => { console.error(error.message); process.exitCode = 1; });
