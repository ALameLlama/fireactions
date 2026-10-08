'use strict';

const fs = require('node:fs');
const state = process.env.STATEFILE;
if (!state) throw new Error('STATEFILE is required');
fs.writeFileSync(`${state}.pre`, 'pre-complete\n');
console.log('javascript-pre-ok');
