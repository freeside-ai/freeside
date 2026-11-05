// A stand-in for `claude -p --input-format stream-json` that the usage
// driver tests run under the driver. It records every stdin line to
// argv[3] and plays the CLI's side of one turn as argv[2] says:
//
//   answer   answer initialize, the user message, and get_usage; exit 0 on EOF
//   silent   never answer get_usage; exit 0 on EOF
//   early    exit 3 right after the initialize answer
//   signal   kill itself with SIGTERM right after the initialize answer
//   fail     answer as `answer` does but exit 2 on EOF
//   mute     never answer initialize; exit 0 on EOF
//   linger   after the initialize answer, leave a detached process holding
//            stdout for twenty-five seconds and exit 4
//
// Its stdout is fixed bytes, including a multibyte character, so the test
// can compare what the driver passed through byte for byte.
'use strict';
const fs = require('node:fs');
const {spawn} = require('node:child_process');

const [mode, record] = process.argv.slice(2);
const out = line => process.stdout.write(line + '\n');
let pending = '';

process.stdin.setEncoding('utf8');
process.stdin.on('data', chunk => {
  pending += chunk;
  for (let end; (end = pending.indexOf('\n')) >= 0;) {
    const line = pending.slice(0, end);
    pending = pending.slice(end + 1);
    fs.appendFileSync(record, line + '\n');
    handle(JSON.parse(line));
  }
});
process.stdin.on('end', () => {
  process.exit(mode === 'fail' ? 2 : 0);
});

function handle(message) {
  if (message.type === 'control_request' && message.request.subtype === 'initialize') {
    if (mode === 'mute') return;
    // Pipe writes are asynchronous on macOS: leave only once the answer
    // has flushed, so the driver sees it before the exit.
    const answer = `{"type":"control_response","response":{"subtype":"success","request_id":"${message.request_id}","response":{}}}\n`;
    process.stdout.write(answer, () => {
      if (mode === 'early') process.exit(3);
      if (mode === 'signal') process.kill(process.pid, 'SIGTERM');
      if (mode === 'linger') {
        spawn(process.execPath, ['-e', 'setTimeout(() => {}, 25000)'],
          {detached: true, stdio: ['ignore', 'inherit', 'ignore']}).unref();
        process.exit(4);
      }
    });
  } else if (message.type === 'user') {
    out('{"type":"system","subtype":"init","note":"café ☕"}');
    out('{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}');
    out('{"type":"result","subtype":"success","is_error":false}');
  } else if (message.type === 'control_request' && message.request.subtype === 'get_usage' && mode !== 'silent') {
    out(`{"type":"control_response","response":{"subtype":"success","request_id":"${message.request_id}","response":{"rate_limits":{}}}}`);
  }
}
