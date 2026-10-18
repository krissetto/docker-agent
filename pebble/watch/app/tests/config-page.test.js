'use strict';
// Executes the real config.html script against a minimal DOM and a scripted Pebble bridge.
const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const html = fs.readFileSync(path.join(__dirname, '..', 'config.html'), 'utf8');
const script = /<script>([\s\S]*?)<\/script>/.exec(html)[1];
const body = /<body>([\s\S]*?)<script>/.exec(html)[1];

class FakeNode {
  constructor(tag) {
    this.tagName = tag.toUpperCase(); this.children = []; this.parent = null; this.listeners = {};
    this.attributes = {}; this.ownText = ''; this.className = ''; this.hidden = false; this.disabled = false;
    this.checked = false; this.type = ''; this.id = ''; this.htmlFor = ''; this._value = '';
  }
  appendChild(child) { child.parent = this; this.children.push(child); return child; }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  getAttribute(name) { return this.attributes[name]; }
  addEventListener(type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); }
  get textContent() { return this.ownText + this.children.map(c => c.textContent).join(''); }
  set textContent(text) { this.children.forEach(c => { c.parent = null; }); this.children = []; this.ownText = String(text); }
  get value() {
    if (this.tagName === 'OPTION') return this._value !== '' || 'value' in this.attributes ? this._value : this.textContent;
    return this._value;
  }
  set value(v) {
    v = String(v);
    if (this.tagName === 'SELECT') { this._value = this.options().some(o => o.value === v) ? v : (this.options()[0] ? this.options()[0].value : ''); return; }
    this._value = v; if (this.tagName === 'OPTION') this.attributes.value = v;
  }
  options() { return this.children.filter(c => c.tagName === 'OPTION'); }
  all() { return [this].concat(...this.children.map(c => c.all())); }
  visible() { for (let n = this; n; n = n.parent) if (n.hidden) return false; return true; }
}

function staticDOM() {
  const root = new FakeNode('body');
  const re = /<(\w+)([^>]*)>/g; let m;
  while ((m = re.exec(body))) {
    const id = /\bid="([^"]+)"/.exec(m[2]); if (!id) continue;
    const n = root.appendChild(new FakeNode(m[1])); n.id = id[1];
    n.disabled = /\sdisabled\b/.test(m[2]); const cls = /class="([^"]*)"/.exec(m[2]); if (cls) n.className = cls[1];
    if (m[1] !== 'select' && m[1] !== 'div') n.textContent = body.slice(re.lastIndex, body.indexOf('<', re.lastIndex));
    if (m[1] === 'select') {
      const inner = body.slice(re.lastIndex, body.indexOf('</select>', re.lastIndex));
      inner.replace(/<option value="([^"]*)">([^<]*)<\/option>/g, (_, v, t) => { const o = n.appendChild(new FakeNode('option')); o.value = v; o.textContent = t; });
      n.value = '0';
    }
  }
  return root;
}

const tick = () => new Promise(resolve => setImmediate(resolve));
async function settle() { for (let i = 0; i < 10; i++) await tick(); }

function load(bridge) {
  const root = staticDOM();
  const timers = [];
  const env = {
    log: [], storage: [], nav: [],
    root, timers,
    byId: id => root.all().find(n => n.id === id),
    text: () => root.textContent
  };
  const location = {};
  let href = '';
  Object.defineProperty(location, 'href', { get: () => href, set: v => { href = v; env.nav.push(v); } });
  const sandbox = {
    document: { getElementById: env.byId, createElement: tag => new FakeNode(tag) },
    location,
    setTimeout: (fn, ms) => { timers.push({ fn, ms }); return timers.length; },
    clearTimeout: id => { if (timers[id - 1]) timers[id - 1].fn = null; },
    console: new Proxy({}, { get: () => (...args) => env.log.push(args) }),
    localStorage: new Proxy({}, { get: (_, k) => () => env.storage.push(k) }),
    unescape, URL
  };
  if (bridge) sandbox.Pebble = bridge;
  vm.runInNewContext(script, sandbox);
  return env;
}

function ipcBridge(handlers) {
  const sent = [];
  return {
    sent,
    targets: () => Promise.resolve([{ id: 'pkjs', name: 'docker-agent' }]),
    sendMessage(target, data) {
      assert.strictEqual(target, 'pkjs');
      sent.push(JSON.parse(JSON.stringify(data)));
      const handler = handlers[data.type];
      return handler ? Promise.resolve(handler(data)) : Promise.resolve({ ok: false, error: 'invalid' });
    }
  };
}

function inputs(env, suffix) { return env.root.all().find(n => n.id === 'rt0-' + suffix); }
function type(node, value) { node.value = value; node.listeners.input.forEach(fn => fn()); }
function change(node, value) { if (node.tagName === 'SELECT') node.value = value; else node.checked = value; node.listeners.change.forEach(fn => fn()); }
function click(node) { assert.ok(!node.disabled, 'clicked a disabled control: ' + node.textContent); assert.ok(node.visible(), 'clicked a hidden control'); node.listeners.click.forEach(fn => fn()); }
function button(env, label, index) { return env.root.all().filter(n => n.tagName === 'BUTTON' && n.textContent === label)[index || 0]; }

const TOKEN = 'sensitive-token-123';
const saved = { id: 'home', name: 'Home lab', base_url: 'https://agent.example', auth_mode: 'bearer', source: 'workspace-source.yaml', root_agent: 'planner' };

function assertNoLeak(env, secret) {
  assert.deepStrictEqual(env.log, [], 'page never logs');
  assert.deepStrictEqual(env.storage, [], 'page never touches localStorage');
  env.nav.forEach(url => assert.ok(!decodeURIComponent(url).includes(secret), 'secret in navigation URL'));
}

(async function () {
  // CSP: bundle-owned page, no external connect, scripts or forms.
  const csp = /Content-Security-Policy" content="([^"]+)"/.exec(html)[1];
  assert.match(csp, /connect-src 'none'/); assert.match(csp, /default-src 'none'/); assert.match(csp, /form-action 'none'/);
  assert.doesNotMatch(html, /\ssrc=|<link|<a\s|<iframe|url\(|@import|window\.open|\.openURL/, 'no external resources or navigation');
  assert.doesNotMatch(script, /XMLHttpRequest|fetch\(|localStorage|console\./, 'no direct network, storage or logging');

  // 1. Ordinary companion without the IPC bridge: legacy, non-secret, explicit and honest.
  {
    const env = load(undefined); await settle();
    assert.match(env.byId('bridge').textContent, /doesn.t support sign-in/);
    const mode = inputs(env, 'mode');
    assert.ok(mode.options().find(o => o.value === 'bearer').disabled, 'bearer option disabled');
    assert.strictEqual(mode.value, 'private_no_auth');
    assert.ok(!inputs(env, 'token').visible() && inputs(env, 'token').disabled, 'token input hidden and disabled');
    assert.ok(!button(env, 'Test connection').visible(), 'no fake test button');
    assert.match(env.text(), /Connection test isn.t available/);
    assert.match(env.text(), /Unchecked here/);
    assert.ok(env.byId('save').disabled);
    type(inputs(env, 'name'), 'Lab'); type(inputs(env, 'url'), 'https://agent.example');
    change(inputs(env, 'trusted'), true); type(inputs(env, 'source'), 'workspace-source.yaml'); type(inputs(env, 'rootname'), 'planner');
    assert.ok(env.byId('save').disabled, 'public host is never saved as no-sign-in');
    assert.match(env.byId('message').textContent, /private or local network/);
    type(inputs(env, 'url'), 'http://192.168.1.20:8080');
    assert.ok(env.byId('save').disabled, 'plain HTTP needs explicit opt-in');
    change(inputs(env, 'http'), true);
    type(inputs(env, 'rootname'), 'x'.repeat(41));
    assert.match(env.byId('message').textContent, /40 bytes/);
    type(inputs(env, 'rootname'), 'planner');
    change(env.byId('alerts'), '2');
    click(env.byId('save'));
    assert.strictEqual(env.nav.length, 1);
    assert.ok(env.nav[0].startsWith('pebblejs://close#'));
    const legacy = JSON.parse(decodeURIComponent(env.nav[0].slice('pebblejs://close#'.length)));
    assert.strictEqual(legacy.alert_mode, 2);
    assert.deepStrictEqual(Object.assign({}, legacy.runtimes[0], { id: 'x' }), { id: 'x', name: 'Lab', base_url: 'http://192.168.1.20:8080', auth_mode: 'private_no_auth', trusted_network: true, allow_insecure_local: true, source: 'workspace-source.yaml', root_agent: 'planner' });
    assert.ok(!('token' in legacy.runtimes[0]));
    assertNoLeak(env, TOKEN);
  }

  // 2. Bridge present but no pkjs target, or a probe that never answers: fail closed to legacy.
  {
    const noTarget = { targets: () => Promise.resolve([{ id: 'other' }]), sendMessage: () => assert.fail('must not send') };
    let env = load(noTarget); await settle();
    assert.match(env.byId('bridge').textContent, /doesn.t support sign-in/);
    const hanging = { targets: () => new Promise(() => {}), sendMessage: () => assert.fail('must not send') };
    env = load(hanging); await settle();
    assert.match(env.byId('bridge').textContent, /Checking/);
    assert.ok(env.byId('save').disabled && env.byId('add').disabled, 'nothing actionable while probing');
    env.timers.find(t => t.ms === 3000).fn(); await settle();
    assert.match(env.byId('bridge').textContent, /doesn.t support sign-in/);
  }

  // 3. IPC: test then save; tests are invalidated by edits; root is explicit; mismatch is surfaced.
  {
    let roots = [{ source: 'workspace-source.yaml', root_agent_name: 'coder', description: 'Writes code' }];
    let pendingTest = null;
    const bridge = ipcBridge({
      get_config: () => ({ ok: true, runtimes: [saved], selected_runtime_id: 'home', alert_mode: 1 }),
      test_runtime: data => pendingTest ? pendingTest(data) : { ok: true, name: 'home-lab', session_api_version: 2 },
      list_roots: () => ({ ok: true, roots, limited: false }),
      runtime_config: () => ({ ok: true })
    });
    const env = load(bridge); await settle();
    assert.match(env.byId('bridge').textContent, /experimental/);
    assert.strictEqual(env.byId('alerts').value, '1', 'saved alert mode shown');
    assert.strictEqual(inputs(env, 'name').value, 'Home lab');
    assert.ok(inputs(env, 'token').visible() && !inputs(env, 'token').disabled);
    assert.strictEqual(inputs(env, 'token').value, '', 'tokens are never prefilled');
    assert.match(env.text(), /Saved choice: planner — workspace-source.yaml\. Test to confirm/);
    assert.ok(env.byId('save').disabled);
    assert.match(env.byId('message').textContent, /Enter the bearer token/);

    type(inputs(env, 'token'), TOKEN);
    click(button(env, 'Test connection')); await settle();
    const test = bridge.sent.find(m => m.type === 'test_runtime');
    assert.strictEqual(test.token, TOKEN);
    assert.ok(!('token' in test.runtime) && !('source' in test.runtime) && !('root_agent' in test.runtime));
    const firstList = bridge.sent.find(m => m.type === 'list_roots');
    assert.strictEqual(firstList.token, TOKEN);
    assert.strictEqual(firstList.source, 'workspace-source.yaml', 'saved source is requested exactly');
    assert.match(env.text(), /Connected to home-lab · session API v2/);
    assert.match(env.text(), /saved choice planner — workspace-source.yaml isn.t offered/, 'mismatch is explicit');
    const rootSelect = inputs(env, 'root');
    assert.strictEqual(rootSelect.value, '', 'single listed root is not auto-selected');
    assert.ok(env.byId('save').disabled);
    assert.match(env.byId('message').textContent, /choose a root agent/);
    change(rootSelect, '0');
    assert.match(env.text(), /Writes code/);
    assert.ok(!env.byId('save').disabled, 'tested + explicit root enables save');

    for (const [field, value] of [['url', 'https://agent2.example'], ['name', 'Home lab 2'], ['token', TOKEN + 'x']]) {
      const old = inputs(env, field).value;
      type(inputs(env, field), value);
      assert.ok(env.byId('save').disabled, field + ' change invalidates the test');
      assert.match(env.text(), /Changed since the last test|Test the connection to list/);
      type(inputs(env, field), old);
      assert.ok(!env.byId('save').disabled, 'restoring the exact tested values is accepted');
    }
    change(inputs(env, 'mode'), 'private_no_auth');
    assert.ok(env.byId('save').disabled, 'auth mode change invalidates the test');
    assert.ok(!inputs(env, 'token').visible());
    change(inputs(env, 'mode'), 'bearer');
    assert.strictEqual(inputs(env, 'token').value, '', 'switching away from bearer drops the typed token');
    type(inputs(env, 'token'), TOKEN);

    // A result for values that changed mid-test is not accepted.
    let release;
    pendingTest = () => new Promise(resolve => { release = () => resolve({ ok: true, session_api_version: 2 }); });
    click(button(env, 'Test connection')); await settle();
    assert.ok(button(env, 'Testing…').disabled);
    type(inputs(env, 'url'), 'https://moved.example');
    release(); await settle();
    assert.match(env.text(), /Changed during the test/);
    type(inputs(env, 'url'), 'https://agent.example');
    assert.ok(env.byId('save').disabled, 'stale test result never counts');
    pendingTest = null;

    // Limited list is explained; there is no free-text fallback in IPC mode.
    roots = [{ source: 'workspace-source.yaml', root_agent_name: 'planner', description: '' }, { source: 'other.yaml', root_agent_name: 'coder' }];
    bridge.handlersLimited = true;
    const limitedBridgeList = bridge.sent.length;
    const origSend = bridge.sendMessage;
    bridge.sendMessage = (t, d) => d.type === 'list_roots' ? (bridge.sent.push(d), Promise.resolve({ ok: true, roots, limited: true })) : origSend.call(bridge, t, d);
    click(button(env, 'Test connection')); await settle();
    assert.ok(bridge.sent.length > limitedBridgeList);
    assert.ok(!('source' in bridge.sent.filter(m => m.type === 'list_roots').pop()), 'a mismatched pin is not requested again');
    assert.match(env.text(), /Only the first 8 root agents are listed/);
    assert.ok(!env.root.all().some(n => n.id === 'rt0-source' || n.id === 'rt0-rootname'), 'no free-text root in IPC mode');
    change(inputs(env, 'root'), '0');
    change(env.byId('alerts'), '0');
    click(env.byId('save')); await settle();
    const cfg = bridge.sent.filter(m => m.type === 'runtime_config').pop();
    assert.deepStrictEqual(cfg, { type: 'runtime_config', selected_runtime_id: 'home', alert_mode: 0,
      runtimes: [{ id: 'home', name: 'Home lab', base_url: 'https://agent.example', auth_mode: 'bearer', trusted_network: false, allow_insecure_local: false, source: 'workspace-source.yaml', root_agent: 'planner', token: TOKEN }] });
    assert.match(env.byId('message').textContent, /^Saved\. Tokens stay in phone memory only/);
    assert.strictEqual(inputs(env, 'token').value, '', 'page drops its token copy after save');
    assert.ok(env.byId('save').disabled, 'a later save needs the token again');
    assert.strictEqual(env.nav.length, 0, 'IPC mode never uses the legacy close URL');
    assertNoLeak(env, TOKEN);

    // The payload is accepted by the real phone handler, which keeps the token out of get_config.
    const listeners = {};
    global.Pebble = { addEventListener: (n, f) => { listeners[n] = f; }, sendAppMessage() {} };
    const store = {};
    global.localStorage = { getItem: k => store[k] || null, setItem: (k, v) => { store[k] = String(v); } };
    global.XMLHttpRequest = function () {};
    const phone = require('../src/pkjs/index.js');
    let reply; phone.runtimeConfigMessage({ data: cfg, respond: r => { reply = r; } });
    assert.deepStrictEqual(reply, { ok: true });
    phone.runtimeConfigMessage({ data: { type: 'get_config' }, respond: r => { reply = r; } });
    assert.ok(!JSON.stringify(reply).includes(TOKEN) && !JSON.stringify(store).includes(TOKEN));
    assert.strictEqual(reply.runtimes[0].root_agent, 'planner'); assert.strictEqual(reply.runtimes[0].source, 'workspace-source.yaml');
    for (const mode of [0, 1, 2]) {
      phone.runtimeConfigMessage({ data: Object.assign({}, cfg, { alert_mode: mode }), respond: r => { reply = r; } });
      assert.deepStrictEqual(reply, { ok: true });
      phone.runtimeConfigMessage({ data: { type: 'get_config' }, respond: r => { reply = r; } });
      assert.strictEqual(reply.alert_mode, mode, 'alert mode ' + mode + ' persists');
    }
    phone.reset();
  }

  // 3b. A pinned source beyond the first 8 is requested exactly and its matching root restored.
  {
    const bridge = ipcBridge({
      get_config: () => ({ ok: true, runtimes: [saved], selected_runtime_id: 'home', alert_mode: 0 }),
      test_runtime: () => ({ ok: true, session_api_version: 2 }),
      list_roots: data => ({ ok: true, limited: false, roots: data.source === 'workspace-source.yaml'
        ? [{ source: 'workspace-source.yaml', root_agent_name: 'planner', description: 'Plans work' }]
        : [{ source: 'other.yaml', root_agent_name: 'coder' }] }),
      runtime_config: () => ({ ok: true })
    });
    const env = load(bridge); await settle();
    type(inputs(env, 'token'), TOKEN);
    click(button(env, 'Test connection')); await settle();
    assert.strictEqual(bridge.sent.find(m => m.type === 'list_roots').source, 'workspace-source.yaml');
    assert.strictEqual(inputs(env, 'root').value, '0', 'saved explicit choice is restored from the fresh list');
    assert.ok(!env.text().includes('isn’t offered') && !env.text().includes('Only the first 8'));
    assert.match(env.text(), /Plans work/);
    click(env.byId('save')); await settle();
    const cfg = bridge.sent.find(m => m.type === 'runtime_config');
    assert.strictEqual(cfg.runtimes[0].source, 'workspace-source.yaml'); assert.strictEqual(cfg.runtimes[0].root_agent, 'planner');
    assertNoLeak(env, TOKEN);
  }

  // 3c. New runtimes never send an invented source filter.
  {
    const bridge = ipcBridge({
      get_config: () => ({ ok: true, runtimes: [], alert_mode: 0 }),
      test_runtime: () => ({ ok: true, session_api_version: 2 }),
      list_roots: () => ({ ok: true, roots: [] })
    });
    const env = load(bridge); await settle();
    type(inputs(env, 'name'), 'Lab'); type(inputs(env, 'url'), 'https://192.168.1.5');
    change(inputs(env, 'mode'), 'private_no_auth'); change(inputs(env, 'trusted'), true);
    click(button(env, 'Test connection')); await settle();
    assert.ok(!('source' in bridge.sent.find(m => m.type === 'list_roots')));
    assert.match(env.text(), /offers no root agents/);
  }

  // 4. Missing list_roots support or failed load fails closed; at most eight runtimes.
  {
    const bridge = ipcBridge({
      get_config: () => ({ ok: true, runtimes: [], alert_mode: 2 }),
      test_runtime: () => ({ ok: true, session_api_version: 2 })
    });
    const env = load(bridge); await settle();
    assert.strictEqual(env.byId('alerts').value, '2');
    assert.strictEqual(inputs(env, 'mode').value, '', 'auth mode is never preselected');
    type(inputs(env, 'name'), 'Lab'); type(inputs(env, 'url'), 'https://192.168.1.5');
    change(inputs(env, 'mode'), 'private_no_auth'); change(inputs(env, 'trusted'), true);
    click(button(env, 'Test connection')); await settle();
    assert.match(env.text(), /can.t list root agents/);
    assert.ok(env.byId('save').disabled);
    for (let i = 1; i < 8; i++) click(env.byId('add'));
    assert.strictEqual(env.root.all().filter(n => n.tagName === 'SECTION').length, 8);
    assert.ok(env.byId('add').disabled); assert.match(env.byId('add').textContent, /Up to 8/);
    click(button(env, 'Remove runtime', 7));
    assert.ok(!env.byId('add').disabled);

    const failing = load(ipcBridge({ get_config: () => { throw new Error('boom'); } })); await settle();
    assert.match(failing.byId('bridge').textContent, /couldn.t be loaded/);
    assert.ok(failing.byId('save').disabled && failing.byId('add').disabled, 'no overwrite after failed load');
  }

  console.log('config page DOM tests passed');
}()).catch(error => { console.error(error); process.exit(1); });
