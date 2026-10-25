"use strict";
// Trusted study wrapper. node:vm supplies a realm and controllable microtask
// drain, not a security boundary. No host function is installed in that realm.
const vm = require("node:vm");
const fs = require("node:fs");
const readline = require("node:readline");
// A derived Promise may reject before a later sequential await attaches its
// handler. Only the returned program Promise determines terminal failure; an
// unhandled-rejection timing heuristic must not change live versus replay.
process.on("unhandledRejection", () => {});
process.on("rejectionHandled", () => {});
const realm = vm.createContext(Object.create(null), {
  codeGeneration: {strings: false, wasm: false}, microtaskMode: "afterEvaluate",
});
const options = {timeout: 500, displayErrors: false};
const driver = vm.runInContext(fs.readFileSync("/realm.js", "utf8"), realm, options);
let started = false;

function enter(expression, values) {
  Object.defineProperty(realm, "__study_driver", {value: driver, configurable: true});
  for (const [name, value] of Object.entries(values)) Object.defineProperty(realm, name, {value, configurable: true});
  // Delete temporary transport globals before entering generated code or
  // draining callbacks. Generated functions have no lexical access to locals.
  const names = ["__study_driver", ...Object.keys(values)];
  const params = names.map((_, i) => `v${i}`);
  const body = names.map(n => `delete globalThis.${n};`).join("");
  return vm.runInContext(`((${params}) => {${body} return ${expression};})(${names})`, realm, options);
}

readline.createInterface({input: process.stdin, crlfDelay: Infinity}).on("line", line => {
  if (line.length > 131072) process.exit(66);
  try {
    const frame = JSON.parse(line);
    if (!started) {
      if (frame.kind !== "start" || typeof frame.source !== "string") process.exit(66);
      started = true;
      const program = vm.runInContext(`(async function(args) {"use strict";\n${frame.source}\n})`, realm, options);
      enter("v0.start(v1,v2)", {__study_program: program, __study_args: JSON.stringify(frame.args)});
    } else enter("v0.deliver(v1)", {__study_delivery: line});
    for (const frame of JSON.parse(driver.take())) process.stdout.write(JSON.stringify(frame) + "\n");
  } catch (_) {
    // Diagnostics never include source, prompt, results or stack contents.
    process.stdout.write('{"kind":"failure","code":"engine_failure"}\n');
    process.exit(65);
  }
});
