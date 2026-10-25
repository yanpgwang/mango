// Independently authored feasibility fixture, not a supported program runtime.
// The returned driver stays outside the program's global scope in both engines.
(() => {
  "use strict";
  const stringify = JSON.stringify;
  const parse = JSON.parse;
  const freeze = Object.freeze;
  const keys = Object.keys;
  const getPrototypeOf = Object.getPrototypeOf;
  const promises = new Map();
  let frames = [];
  let next = 0;
  let terminal = null;

  function json(value, depth = 0, seen = new Set()) {
    if (depth > 32) throw new Error("value_depth");
    if (value === null || typeof value === "string" || typeof value === "boolean") return value;
    if (typeof value === "number" && Number.isFinite(value)) return value;
    if (typeof value !== "object" || seen.has(value)) throw new Error("non_json_value");
    seen.add(value);
    let result;
    if (Array.isArray(value)) result = value.map(v => json(v, depth + 1, seen));
    else {
      if (getPrototypeOf(value) !== Object.prototype && getPrototypeOf(value) !== null) throw new Error("non_json_value");
      result = Object.create(null);
      for (const key of keys(value).sort()) {
        const descriptor = Object.getOwnPropertyDescriptor(value, key);
        if (!descriptor || !("value" in descriptor)) throw new Error("non_json_value");
        result[key] = json(descriptor.value, depth + 1, seen);
      }
    }
    seen.delete(value);
    return result;
  }

  function encoded(value) {
    const text = stringify(json(value));
    if (text.length > 32768) throw new Error("value_size");
    return text;
  }

  function agent(prompt, options = {}) {
    // The trusted host validates every frame too. Script-side checks are only
    // deterministic pure computation, never an authorization decision.
    if (next >= 128) throw new Error("command_limit");
    const args = parse(encoded({prompt, options}));
    const id = ++next;
    const promise = new Promise((resolve, reject) => promises.set(id, {resolve, reject}));
    // Keep a result rejected before the program reaches its await from becoming
    // a Node process-wide unhandled rejection. Preserve the original Promise.
    promise.then(undefined, () => {});
    frames.push({kind: "invoke", id, args});
    return promise;
  }
  Object.defineProperty(globalThis, "agent", {value: freeze(agent)});

  // Do not supply a clock, randomness, modules, timers or direct I/O. This is
  // a language profile; the disposable container is the containment boundary.
  for (const name of ["Date", "Intl", "WeakRef", "FinalizationRegistry", "SharedArrayBuffer", "Atomics",
    "console", "performance", "fetch", "process", "require", "Buffer", "WebAssembly",
    "setTimeout", "setInterval", "setImmediate", "queueMicrotask", "eval", "Function"]) {
    Object.defineProperty(globalThis, name, {value: undefined, writable: false, configurable: false});
  }
  for (const prototype of [getPrototypeOf(function () {}), getPrototypeOf(async function () {}),
    getPrototypeOf(function* () {}), getPrototypeOf(async function* () {})]) {
    Object.defineProperty(prototype, "constructor", {value: undefined, writable: false, configurable: false});
  }
  for (const name of ["race", "any"]) Object.defineProperty(Promise, name, {value: undefined});
  Object.defineProperty(Math, "random", {value: undefined});
  for (const name of ["Object", "Array", "String", "Number", "Boolean", "RegExp", "Promise", "Error", "TypeError",
    "RangeError", "SyntaxError", "Map", "Set", "JSON", "Math", "Reflect"]) {
    const intrinsic = globalThis[name];
    if (intrinsic.prototype) freeze(intrinsic.prototype);
    freeze(intrinsic);
    Object.defineProperty(globalThis, name, {value: intrinsic, writable: false, configurable: false});
  }
  Object.defineProperty(globalThis, "globalThis", {value: globalThis, writable: false, configurable: false});

  return {
    start(program, args) {
      program(parse(args)).then(value => {
        try {
          const copied = parse(encoded(value));
          terminal = promises.size ? {kind: "failure", code: "outstanding_invocations"} : {kind: "return", value: copied};
        } catch (_) { terminal = {kind: "failure", code: "invalid_return"}; }
      }, () => { terminal = {kind: "failure", code: "program_error"}; });
    },
    deliver(text) {
      const frame = parse(text);
      const pending = promises.get(frame.id);
      if (!pending) throw new Error("invalid_delivery");
      promises.delete(frame.id);
      if (frame.error) {
        const error = new Error(frame.error.code);
        error.code = frame.error.code;
        pending.reject(error);
      } else pending.resolve(parse(encoded(frame.value)));
    },
    take() {
      const result = frames;
      frames = [];
      result.push(terminal || (promises.size ? {kind: "wait", pending: [...promises.keys()]} : {kind: "failure", code: "orphan_promise"}));
      return stringify(result);
    },
  };
})()
