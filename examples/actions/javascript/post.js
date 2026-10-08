'use strict';

const fs = require('node:fs');
const state = process.env.STATEFILE;
if (!state || fs.readFileSync(state, 'utf8') !== 'main-complete\n') {
  throw new Error('JavaScript main state was not preserved');
}
if (fs.readFileSync(`${state}.pre`, 'utf8') !== 'pre-complete\n') {
  throw new Error('JavaScript pre state was not preserved');
}
if (process.env.STATE_main_proof !== 'main-state-value') {
  throw new Error('Runner did not preserve action state for post');
}
console.log('javascript-post-ok');
