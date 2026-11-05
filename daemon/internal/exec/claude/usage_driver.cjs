// The file_v6 writer's usage driver. It runs under the same privilege drop
// as the CLI and holds nothing the CLI does not: it reads the prompt from
// stdin, runs the argv it was given, and copies the CLI's stdout to its own
// unchanged, so the transcript is still the CLI's alone.
//
// It drives one turn over the CLI's stream-json input: initialize, the
// prompt as one user message, and after the turn's result exactly one
// get_usage request, which the CLI answers from that turn's rate-limit
// headers. It then closes the CLI's stdin so the CLI exits. Nothing here
// infers, reads a file, or prints the environment. Usage is optional: an
// unanswered get_usage closes stdin after ANSWER_TIMEOUT_MS, and the exit
// status is always the CLI's. A CLI that never answers initialize gets EOF
// after INIT_TIMEOUT_MS, as a file_v5 CLI would see it at once. Once the CLI
// exits, the driver waits at most EXIT_GRACE_MS for its stdout to drain, so
// a descendant still holding that pipe cannot keep the stage running. The
// prompt is the renderer's complete UTF-8, so decoding it keeps every byte.
// The request ids are the ones the daemon's transcript reader matches
// (exec.ClaudeUsageRequestID).
'use strict';
const {spawn} = require('node:child_process');
const {constants} = require('node:os');
const {StringDecoder} = require('node:string_decoder');

const INIT_ID = 'usage-init';
const USAGE_ID = 'usage-read';
const ANSWER_TIMEOUT_MS = 15000;
const INIT_TIMEOUT_MS = 120000;
const EXIT_GRACE_MS = 2000;

function readStdin() {
  return new Promise((resolve, reject) => {
    const chunks = [];
    process.stdin.on('data', chunk => chunks.push(chunk));
    process.stdin.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    process.stdin.on('error', reject);
  });
}

async function main() {
  const [command, ...args] = process.argv.slice(2);
  const prompt = await readStdin();
  const child = spawn(command, args, {stdio: ['pipe', 'pipe', 'inherit']});
  for (const signal of ['SIGHUP', 'SIGINT', 'SIGTERM']) process.on(signal, () => child.kill(signal));
  // A CLI that exits early closes its stdin; a late write must not crash
  // the driver before it reports the CLI's status.
  child.stdin.on('error', () => {});
  const send = message => child.stdin.write(JSON.stringify(message) + '\n');
  let prompted = false, resulted = false, closed = false, timer;
  const close = () => {
    if (closed) return;
    closed = true;
    clearTimeout(timer);
    child.stdin.end();
  };
  timer = setTimeout(close, INIT_TIMEOUT_MS);
  const handle = line => {
    let message;
    try { message = JSON.parse(line); } catch { return; }
    if (message === null || typeof message !== 'object') return;
    const id = message.type === 'control_response' ? message.response?.request_id : undefined;
    if (id === INIT_ID && !prompted && !closed) {
      prompted = true;
      clearTimeout(timer);
      send({type: 'user', message: {role: 'user', content: prompt}});
    } else if (message.type === 'result' && prompted && !resulted) {
      resulted = true;
      send({type: 'control_request', request_id: USAGE_ID, request: {subtype: 'get_usage'}});
      timer = setTimeout(close, ANSWER_TIMEOUT_MS);
    } else if (id === USAGE_ID && resulted) {
      close();
    }
  };
  // The bytes pass through untouched; only this decoded copy is parsed, and
  // the decoder keeps a character split across chunks whole.
  const decoder = new StringDecoder('utf8');
  let pending = '';
  child.stdout.on('data', chunk => {
    process.stdout.write(chunk);
    pending += decoder.write(chunk);
    for (let end; (end = pending.indexOf('\n')) >= 0;) {
      const line = pending.slice(0, end);
      pending = pending.slice(end + 1);
      handle(line);
    }
  });
  send({type: 'control_request', request_id: INIT_ID, request: {subtype: 'initialize'}});
  const [code, signal] = await new Promise(resolve => {
    child.on('error', () => resolve([127, null]));
    child.on('exit', (code, signal) => {
      let grace;
      const done = () => { clearTimeout(grace); resolve([code, signal]); };
      if (child.stdout.closed) return done();
      child.stdout.on('close', done);
      grace = setTimeout(() => { child.stdout.destroy(); done(); }, EXIT_GRACE_MS);
    });
  });
  clearTimeout(timer);
  process.exitCode = signal ? 128 + (constants.signals[signal] ?? 0) : code;
}

main().catch(() => { process.exitCode = 1; });
