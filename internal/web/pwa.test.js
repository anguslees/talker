import test from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import vm from "node:vm";

const workerSource = await readFile(new URL("./assets/sw.js", import.meta.url), "utf8");
const registrationSource = await readFile(new URL("./assets/pwa.js", import.meta.url), "utf8");
const origin = "https://talker.example";

function worker(fetch) {
  const listeners = new Map();
  const forbidden = () => { throw new Error("Workers must not store data or take over active sessions"); };
  const context = {
    URL, Response, fetch,
    location: { origin },
    addEventListener: (type, listener) => listeners.set(type, listener),
    get caches() { return forbidden(); },
    get indexedDB() { return forbidden(); },
    skipWaiting: forbidden,
    clients: { claim: forbidden },
  };
  context.self = context;
  vm.runInNewContext(workerSource, context, { filename: "sw.js" });
  assert.deepEqual([...listeners.keys()], ["fetch"], "no install/activate takeover or precache");
  return request => {
    let response;
    listeners.get("fetch")({ request, respondWith: value => { response = value; } });
    return response;
  };
}

function navigation(path = "/", overrides = {}) {
  return { url: new URL(path, origin).href, method: "GET", mode: "navigate", ...overrides };
}

test("worker fetches root navigations without caching and preserves the original request", async () => {
  for (const path of ["/", "/?source=installed"]) {
    const request = navigation(path, { credentials: "include", redirect: "follow" });
    const response = new Response("online console");
    let calls = 0;
    const dispatch = worker(async (actual, options) => {
      calls++;
      assert.equal(actual, request);
      assert.equal(options.cache, "no-store");
      return response;
    });
    assert.equal(await dispatch(request), response);
    assert.equal(calls, 1);
  }
});

test("worker preserves HTTP errors and authentication redirects instead of serving the app", async () => {
  for (const status of [302, 401, 403, 404, 500, 503]) {
    const response = new Response("server response", { status, headers: { Location: "/auth/login" } });
    const dispatch = worker(async () => response);
    assert.equal(await dispatch(navigation()), response);
  }
});

test("worker returns a self-contained, non-cacheable offline page only after network failure", async () => {
  const dispatch = worker(async () => { throw new TypeError("Failed to fetch"); });
  const response = await dispatch(navigation());
  assert.equal(response.status, 503);
  assert.equal(response.headers.get("Content-Type"), "text/html; charset=utf-8");
  assert.equal(response.headers.get("Cache-Control"), "no-store");
  assert.equal(response.headers.get("X-Content-Type-Options"), "nosniff");
  assert.match(response.headers.get("Content-Security-Policy"), /default-src 'none'/);
  assert.match(response.headers.get("Content-Security-Policy"), /base-uri 'none'/);
  const page = await response.text();
  assert.match(page, /Talker is offline/);
  assert.match(page, /connection for voice and background tasks/);
  assert.match(page, /<a href="\/">Try again<\/a>/);
  assert.match(page, /#101415/);
  assert.doesNotMatch(page, /<script|<link|<img|<iframe|<form|http-equiv|url\(/i);
});

test("worker never intercepts API, credentials, transcripts, audio, or other subresources", () => {
  const dispatch = worker(() => { throw new Error("fetch must not be called"); });
  for (const path of [
    "/api/live/token", "/api/control", "/api/tasks", "/api/tasks/123", "/api/events",
    "/api/transcripts", "/auth/login", "/auth/callback?code=secret", "/login",
    "/app.js", "/styles.css", "/audio.js", "/mic-worklet.js", "/playback-worklet.js",
    "/live.js", "/pwa.js", "/sw.js", "/manifest.webmanifest", "/icon-192.png", "/recording.pcm",
    "/index.html", "/missing", "https://other.example/",
    "https://generativelanguage.googleapis.com/?access_token=secret",
  ]) {
    for (const mode of ["navigate", "same-origin", "cors", "no-cors"]) {
      for (const method of ["GET", "POST"]) {
        assert.equal(dispatch(navigation(path, { method, mode })), undefined, `${method} ${mode} ${path}`);
      }
    }
  }
  for (const method of ["POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]) {
    assert.equal(dispatch(navigation("/", { method })), undefined, method);
  }
  for (const mode of ["same-origin", "cors", "no-cors", "websocket"]) {
    assert.equal(dispatch(navigation("/", { mode })), undefined, mode);
  }
});

test("registration is optional outside secure contexts or supported browsers", () => {
  for (const isSecureContext of [false, true]) {
    vm.runInNewContext(registrationSource, { window: { isSecureContext }, navigator: {} });
  }
  vm.runInNewContext(registrationSource, {
    window: { isSecureContext: false },
    navigator: { serviceWorker: { register() { throw new Error("insecure registration"); } } },
  });
});

test("registration uses the root scope and fresh worker code without reloading or messaging clients", async () => {
  let calls = 0;
  const pendingWorker = { postMessage() { throw new Error("must not force activation"); } };
  await vm.runInNewContext(registrationSource, {
    window: { isSecureContext: true },
    navigator: { serviceWorker: {
      register: async (url, options) => {
        calls++;
        assert.equal(url, "/sw.js");
        assert.equal(options.scope, "/");
        assert.equal(options.updateViaCache, "none");
        return { waiting: pendingWorker, active: pendingWorker };
      },
      addEventListener() { throw new Error("must not reload on controllerchange"); },
    } },
  });
  assert.equal(calls, 1);
});

test("failed registration is handled without interrupting the console or logging credentials", async () => {
  const warnings = [];
  await vm.runInNewContext(registrationSource, {
    window: { isSecureContext: true },
    navigator: { serviceWorker: { register: async () => { throw new Error("auth?access_token=secret"); } } },
    console: { warn: (...args) => warnings.push(args) },
  });
  assert.equal(warnings.length, 1);
  assert.deepEqual(warnings[0], ["Talker could not enable offline support."]);
});
