// Offline authored/effective descriptor checks using a separately obtained pinned schema tree.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const Ajv = require('ajv');
const yaml = require('js-yaml');
const root = process.env.SPEC_DIR;
if (!root && !process.env.SPEC_SCHEMA) throw new Error('Set SPEC_DIR to the pinned sandbox-kit-spec checkout');
const schema = process.env.SPEC_SCHEMA || path.join(root, 'schema');
const ajv = new Ajv({allErrors: true, strict: false});
function add(dir) {
  for (const entry of fs.readdirSync(dir, {withFileTypes: true})) {
    const file = path.join(dir, entry.name);
    if (entry.isDirectory()) add(file);
    else if (entry.name.endsWith('.json')) {
      ajv.addSchema(JSON.parse(fs.readFileSync(file, 'utf8')),
        'https://spec.invalid/schema/' + path.relative(schema, file).split(path.sep).join('/'));
    }
  }
}
add(schema);
const validate = ajv.getSchema('https://spec.invalid/schema/kit.schema.json');
function valid(descriptor) {
  if (!validate(descriptor)) throw new Error(JSON.stringify(validate.errors));
  return descriptor;
}
function read(file) { return valid(yaml.load(fs.readFileSync(file, 'utf8'))); }
const workload = read('kit/async-agent.yaml');
assert.deepEqual(workload.capabilities.filter(c => c.type === 'com.docker.sandbox/host-mount@1'), [
  {type: 'com.docker.sandbox/host-mount@1', config: {path: '/home/agent/async-agent-sessions', mode: '0700'}},
]);
const contextType = 'com.docker.sandbox/agent-context@2';
const context = workload.capabilities.filter(c => c.type === contextType);
assert.deepEqual(context, [{type: contextType, optional: true, config: {filename: 'ASYNC_AGENT_KIT.md'}}]);
// The authored request was delivery: inline, not progressive. In d841dec,
// delivery defaults to progressive for MIXIN bodies only: workload guidance
// always appears in the root profile (SPEC-v3 §9.5). The m.11/m.12 contract
// has no delivery switch: every selected body is inline in every profile.
// Filename-only is their common representation, retaining sibling placement.
const contextSchema = ajv.getSchema('https://spec.invalid/schema/capabilities/com.docker.sandbox/agent-context@2.schema.json').schema;
const legacyInline = structuredClone(workload);
legacyInline.capabilities.find(c => c.type === contextType).config.delivery = 'inline';
if (contextSchema.properties.delivery) {
  assert.equal(contextSchema.properties.delivery.default, 'progressive');
  assert.match(contextSchema.properties.delivery.description, /workload guidance always appears in the root profile/);
  valid(legacyInline);
} else {
  assert.match(contextSchema.description, /Inline guidance retained in every effective profile/);
  assert.throws(() => valid(legacyInline), /delivery/);
}
const names = fs.readdirSync('kit/opt-ins');
const mixins = Object.fromEntries(names.map(name => [name, read(`kit/opt-ins/${name}/${name}.yaml`)]));
if (process.argv.includes('--context-only')) {
  console.log('Schema ' + schema + ': six authored descriptors and inline workload context compatibility passed');
  process.exit(0);
}
// Structural expansion mirrors the documented whole-scalar integer/boolean adoption,
// not serialized text replacement. This is not a replacement for the runtime merger.
function effective(input, supplied = {}) {
  const values = {};
  for (const [name, arg] of Object.entries(input.args || {})) {
    const value = supplied[name] ?? arg.default;
    if (value === undefined) throw new Error('Missing required argument: ' + name);
    if (arg.enum && !arg.enum.includes(value)) throw new Error('Invalid enum: ' + name);
    if (arg.pattern && !(new RegExp(arg.pattern)).test(value)) throw new Error('Invalid pattern: ' + name);
    values[name] = value;
    if (arg.env) values['env.' + arg.env] = value;
  }
  function expand(value) {
    if (Array.isArray(value)) return value.map(expand);
    if (value && typeof value === 'object') return Object.fromEntries(Object.entries(value).map(([k, v]) => [k, expand(v)]));
    if (typeof value !== 'string') return value;
    const result = value.replace(/\$\{\{ kit\.(args|env)\.([A-Za-z_][A-Za-z0-9_]*) \}\}/g,
      (_, kind, name) => values[kind === 'env' ? 'env.' + name : name]);
    if (/^\$\{\{ kit\.args\./.test(value) && /^([1-9][0-9]*|0)$/.test(result)) return Number(result);
    return result;
  }
  return valid(expand(input));
}
const selected = {
  'docker-auth': {registry: 'ghcr.io'},
  'host-port': {hostPort: '5432', guestPort: '15432'},
};
const resolved = Object.fromEntries(names.map(name => [name, effective(mixins[name], selected[name])]));
assert.deepEqual(resolved['docker-auth'].capabilities.map(c => c.config), [
  {registries: ['ghcr.io']}, {runtime: {allow: ['ghcr.io:443']}},
]);
assert.deepEqual(resolved['host-port'].capabilities[0].config,
  {host: 5432, container: 15432, transport: 'tcp', family: 'ipv4'});
for (const registry of ['', '*', '*.example.com', 'https://ghcr.io', 'ghcr.io:443', 'ghcr.io/path', 'GHCR.io', '127.0.0.1', 'a'.repeat(254) + '.io']) {
  assert.throws(() => effective(mixins['docker-auth'], {registry}), registry);
}
assert.throws(() => effective(mixins['docker-auth']));
for (const supplied of [
  {}, {hostPort: '0', guestPort: '15432'}, {hostPort: '65536', guestPort: '15432'},
  {hostPort: '5432', guestPort: '080'}, {hostPort: '5432', guestPort: '15432', transport: 'sctp'},
  {hostPort: '5432', guestPort: '15432', family: 'any'},
]) assert.throws(() => effective(mixins['host-port'], supplied));
// Bounded reconciliation for these fixtures, not a runtime merger. Pinned spec
// d841decb96d9d676a3813ad46cd05583591cdcb3, SPEC-v3 §7.1/§9.5 and
// docs/spec/capabilities/com.docker.sandbox/dockerd@1.md#composition:
// dockerd is singleton, autoStart unions (omitted means true), required wins;
// network-policy allow/deny union per phase; lifecycle hooks concatenate.
function combine(contributions) {
  const combined = structuredClone(contributions[0]);
  const overlapping = new Set(['dockerd', 'network-policy', 'lifecycle'].map(n => 'com.docker.sandbox/' + n + '@1'));
  for (const contribution of contributions.slice(1)) for (const cap of contribution.capabilities) {
    const prior = overlapping.has(cap.type) && combined.capabilities.find(c => c.type === cap.type);
    if (!prior) {
      combined.capabilities.push(structuredClone(cap));
      continue;
    }
    if (cap.type === 'com.docker.sandbox/dockerd@1') {
      prior.config = {autoStart: prior.config?.autoStart !== false || cap.config?.autoStart !== false};
    } else if (cap.type === 'com.docker.sandbox/network-policy@1') {
      for (const [phase, rules] of Object.entries(cap.config)) {
        const target = prior.config[phase] ||= {};
        for (const [rule, hosts] of Object.entries(rules)) {
          assert.ok(['allow', 'deny'].includes(rule), 'fixture merger only handles allow/deny host lists');
          target[rule] = [...new Set([...(target[rule] || []), ...hosts])];
        }
      }
    } else {
      for (const [hook, commands] of Object.entries(cap.config)) {
        assert.ok(['install', 'startup'].includes(hook), 'fixture merger only handles install/startup hooks');
        prior.config[hook] = [...(prior.config[hook] || []), ...structuredClone(commands)];
      }
    }
    if (prior.optional === true && cap.optional === true) prior.optional = true;
    else delete prior.optional;
  }
  return valid(combined);
}
const dockerType = 'com.docker.sandbox/dockerd@1';
const dockerEntries = descriptor => descriptor.capabilities.filter(c => c.type === dockerType);
assert.deepEqual(dockerEntries(effective(workload)), [{type: dockerType, optional: true, config: {autoStart: true}}]);
// Exercise the spec's autoStart/default and optionality rules independently of
// the authored pair, including reversed contribution order.
for (const left of [false, true, undefined]) for (const right of [false, true, undefined]) {
  for (const leftOptional of [false, true]) for (const rightOptional of [false, true]) {
    const descriptor = (autoStart, optional) => ({schemaVersion: '3', kind: 'mixin', capabilities: [
      {type: dockerType, ...(optional ? {optional: true} : {}), ...(autoStart === undefined ? {} : {config: {autoStart}})},
    ]});
    const merged = combine([descriptor(left, leftOptional), descriptor(right, rightOptional)]);
    assert.deepEqual(dockerEntries(merged), [{type: dockerType,
      ...(leftOptional && rightOptional ? {optional: true} : {}),
      config: {autoStart: left !== false || right !== false}}]);
  }
}
// Every subset has one effective Docker request, never conflicting singletons.
for (let mask = 0; mask < (1 << names.length); mask++) {
  const chosen = names.filter((_, i) => mask & (1 << i));
  const combined = combine([effective(workload), ...chosen.map(name => resolved[name])]);
  const docker = dockerEntries(combined);
  assert.equal(docker.length, 1);
  assert.equal(docker[0].optional === true, !chosen.includes('dockerd'));
  assert.equal(docker[0].config.autoStart, true);
  for (const type of ['network-policy', 'lifecycle']) {
    assert.equal(combined.capabilities.filter(c => c.type === 'com.docker.sandbox/' + type + '@1').length, 1);
  }
  const network = combined.capabilities.find(c => c.type === 'com.docker.sandbox/network-policy@1');
  assert.equal(network.config.runtime.allow.includes('ghcr.io:443'), chosen.includes('docker-auth'));
  const lifecycle = combined.capabilities.find(c => c.type === 'com.docker.sandbox/lifecycle@1');
  const baseLifecycle = workload.capabilities.find(c => c.type === lifecycle.type);
  assert.equal(lifecycle.config.install.length, baseLifecycle.config.install.length + Number(chosen.includes('retained-state')));
  assert.deepEqual(lifecycle.config.startup, effective(workload).capabilities.find(c => c.type === lifecycle.type).config.startup);
  assert.equal(combined.capabilities.filter(c => c.type === 'com.docker.sandbox/agent-context@2').length, 1);
  assert.equal(combined.capabilities.filter(c => c.type === 'com.docker.sandbox/agent-context@1').length, 0);
}
console.log('Schema ' + schema + ': six authored descriptors, inline workload context compatibility, effective inputs, rejection cases, Docker merge rules, and 32 reconciled selection subsets passed');
