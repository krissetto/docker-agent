// Run: node cmd/wasm/authority_lifecycle_test.js /tmp/docker-agent.wasm "$(go env GOROOT)"
"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const nodeProcess = process;
globalThis.crypto ??= require("node:crypto").webcrypto;
require(nodeProcess.argv[3] + "/lib/wasm/wasm_exec.js");

const NativePromise = Promise;
const executors = [];
globalThis.Promise = class extends NativePromise {
  constructor(executor) {
    executors.push(executor);
    super(executor);
  }
};
const frame = (value) => "data: " + JSON.stringify(value) + "\n\n";
const snapshot = frame({version: 2, type: "snapshot", snapshot: {
  session: {id: "s"}, status: {session_id: "s"}, epoch: "e", cursor: 0,
}}) + frame({version: 2, type: "ready", cursor: 0});
let pending = false;
globalThis.fetch = (url, options) => new NativePromise((resolve, reject) => {
  const aborted = () => reject(new Error("aborted"));
  options.signal?.addEventListener("abort", aborted, {once: true});
  if (options.signal?.aborted) return aborted();
  if (pending && !url.endsWith("/events")) return;
  setTimeout(() => {
    if (options.signal?.aborted) return;
    if (url.endsWith("/events")) {
      const body = new ReadableStream({
        start(controller) {
          controller.enqueue(new TextEncoder().encode(snapshot));
          options.signal?.addEventListener("abort", () => controller.close(), {once: true});
        },
      });
      resolve(new Response(body, {headers: {"Content-Type": "text/event-stream"}}));
    } else {
      resolve(new Response('{"ok":true}', {headers: {"Content-Type": "application/json"}}));
    }
  }, 1);
});

const tick = () => new NativePromise((resolve) => setTimeout(resolve, 10));
const released = (callback) => {
  const original = console.error;
  let diagnostic = "";
  console.error = (...args) => { diagnostic += args.join(" "); };
  try { callback(); } finally { console.error = original; }
  assert.match(diagnostic, /call to released function/);
};

(async () => {
  const go = new Go();
  go.argv = ["docker-agent.wasm"];
  const {instance} = await WebAssembly.instantiate(fs.readFileSync(nodeProcess.argv[2]), go.importObject);
  // net/http disables Fetch in Node; this harness deliberately models a browser.
  delete globalThis.process;
  void go.run(instance);
  while (!globalThis.dockerAgent) await tick();
  globalThis.process = nodeProcess;

  const api = globalThis.dockerAgent;
  const authority = api.connectAuthority({url: "https://authority.invalid"});
  const before = executors.length;
  assert.deepEqual(await authority.request("GET", "/s/status"), {ok: true});
  released(executors[before]); // async completion did not need the Go executor
  await assert.rejects(authority.request("GET", "//invalid"), /invalid/);

  const throwing = authority.observe("s", () => { throw new Error("listener failed"); });
  await assert.rejects(throwing.done, /listener failed/);
  throwing.close();
  throwing.close();

  let selfClosing;
  selfClosing = authority.observe("s", () => selfClosing.close());
  await selfClosing.done;
  selfClosing.close();
  await tick();

  for (let i = 0; i < 30; i++) {
    const client = api.connectAuthority({url: "https://authority.invalid", token: "ephemeral"});
    const methods = [client._request, client._observe, client._disconnect];
    let subscription;
    await new NativePromise((resolve) => {
      subscription = client.observe("s", () => resolve());
    });
    const close = subscription._close;
    pending = true;
    const request = client.request("GET", "/s/status");
    const requestDone = assert.rejects(request, /aborted|canceled/);
    client.disconnect();
    client.disconnect();
    await requestDone;
    await subscription.done;
    subscription.close();
    subscription.close();
    await tick();
    await assert.rejects(client.request("GET", "/s/status"), /disconnected/);
    assert.throws(() => client.observe("s", () => {}), /disconnected/);
    for (const method of methods) released(method);
    released(close);
    pending = false;
  }
  authority.disconnect();
  await tick();
  assert.equal(typeof api.parseConfig("agents:\n  root:\n    model: openai/test\n    instruction: hello\n").agents[0].name, "string");
  console.log("OK — Promise executor release, async requests, callback throws, repeated disconnect, inflight drain, subscription/method release, and global exports.");
  nodeProcess.exit(0);
})().catch((error) => {
  console.error(error);
  nodeProcess.exit(1);
});
