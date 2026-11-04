// Rebuilds "Settings, Usage, New Task - 4 Nov 2026.dc.html" beside this folder. Run from the handoff folder:  node _gen/build.js
const fs = require('fs'); const path = require('path'); const vm = require('vm');
const here = __dirname; const out = path.resolve(here, '..', 'Settings, Usage, New Task - 4 Nov 2026.dc.html');
const src = ['lib.js', 'frames.js', 'nov.js', 'var.js', 'var2.js', 'var3.js', 'var5.js', 'var6.js'].map((f) => fs.readFileSync(path.join(here, f), 'utf8')).join('\n');
const ctx = vm.createContext({}); vm.runInContext(src + '\nthis.__out = buildVar6();', ctx, { filename: 'handoff-build.js' });
fs.writeFileSync(out, ctx.__out); console.log('wrote', out, ctx.__out.length);
