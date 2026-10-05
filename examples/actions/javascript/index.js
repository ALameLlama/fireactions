'use strict';

const fs = require('node:fs');
const state = process.env.STATEFILE;
if (!state) throw new Error('STATEFILE is required');
fs.writeFileSync(state, 'main-complete\n');
fs.appendFileSync(process.env.FORGEJO_ENV, 'FIREACTIONS_JS=main-ok\n');
fs.appendFileSync(process.env.FORGEJO_STATE, 'main_proof=main-state-value\n');
console.log('javascript-main-ok');
