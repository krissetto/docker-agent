'use strict';

const fs = require('fs');
const path = require('path');
const root = path.resolve(__dirname, '..');
const html = fs.readFileSync(path.join(root, 'config.html'), 'utf8');
const output = "'use strict';\nmodule.exports = " + JSON.stringify(html) + ';\n';
fs.writeFileSync(path.join(root, 'src/pkjs/config-page.js'), output);
