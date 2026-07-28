// Babel's fixture protocol, reimplemented against the same rules its own
// helpers use (packages/babel-helper-fixtures, packages/
// babel-helper-transform-fixture-test-runner): a fixtures root holds SUITE
// directories, each suite holds TASK directories, and a task is
// input.<ext> (+ options.json) compared against output.<ext>. Options come from
// the fixtures root, are REPLACED by a suite's own options.json, and are
// shallow-merged with the task's.
//
// It lives here rather than in Go because the merge, the plugin-name
// resolution and the transform all have to happen in the runtime under test —
// which is the point: every fixture is @babel/core doing real work on this
// engine.
//
// The host drives it as: __babeltest_run(fixturesRoot) -> JSON string.
//
// It is an ES MODULE because @babel/core 8 is one ("type": "module", no CJS
// entry). Node >= 22 can require() a synchronous ES module; this runtime cannot
// yet, so the harness imports Babel the way a modern application does.
import fs from "node:fs";
import path from "node:path";
import * as babel from "@babel/core";
import { parse } from "@babel/parser";
import generate from "@babel/generator";

// Options keys that belong to the fixture protocol, not to Babel. Babel
// validates its options strictly, so these must not reach it.
const HARNESS_KEYS = new Set([
  "BABEL_8_BREAKING",
  "DO_NOT_SET_SOURCE_TYPE",
  "SKIP_ON_PUBLISH",
  "SKIP_babel7plugins_babel8core",
  "externalHelpers",
  "ignoreOutput",
  "minNodeVersion",
  "minNodeVersionTransform",
  "os",
  "throws",
  "validateLogs",
  "validateSourceMapVisual",
]);

// The version Babel's own fixture runner declares for the external-helpers
// bundle (packages/babel-helper-transform-fixture-test-runner).
const EXTERNAL_HELPERS_VERSION = "7.100.0";

const INPUT_EXTS = [".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx", ".cts", ".mts"];

function readFile(p) {
  try {
    // Babel's readFile trims trailing whitespace; expected output is compared
    // in that form.
    return fs.readFileSync(p, "utf8").replace(/\s+$/, "");
  } catch (e) {
    return "";
  }
}

function readJSON(p) {
  const src = readFile(p);
  if (!src) return null;
  return JSON.parse(src);
}

function isDir(p) {
  try {
    return fs.statSync(p).isDirectory();
  } catch (e) {
    return false;
  }
}

function findFile(dir, base) {
  for (const ext of INPUT_EXTS) {
    const p = path.join(dir, base + ext);
    try {
      fs.statSync(p);
      return p;
    } catch (e) {
      /* next */
    }
  }
  return null;
}

// resolvePackages rewrites a fixture's plugin/preset references into ones this
// installation can load, mirroring babel-helper-fixtures'
// resolveOptionPluginOrPreset + wrapPackagesArray:
//
//   - "./plugin.js"          -> absolute, relative to the options.json's dir
//   - "transform-x"          -> "@babel/plugin-transform-x"
//   - "@babel/transform-x"   -> "@babel/plugin-transform-x"
//   - "module:whatever"      -> "whatever", taken literally
//
// Babel's own harness maps the short name onto the MONOREPO path
// (packages/babel-plugin-transform-x/lib/index.js); the published-package
// equivalent is the scoped name. Without this the short names reach Babel's
// config loader, which standardizes "transform-x" to the unscoped
// "babel-plugin-transform-x" — a package that does not exist on npm — and every
// fixture fails to resolve its plugin.
function resolvePackages(options, optionsDir) {
  if (isGeneratorCorpus) return options;
  for (const sub of options.overrides || []) resolvePackages(sub, optionsDir);
  for (const envName of Object.keys(options.env || {})) {
    resolvePackages(options.env[envName], optionsDir);
  }
  for (const type of ["plugin", "preset"]) {
    const key = type + "s";
    const list = options[key];
    if (!Array.isArray(list)) continue;
    options[key] = list.map((entry) => {
      const val = Array.isArray(entry) ? entry.slice() : [entry];
      if (typeof val[0] === "string") val[0] = packageFor(type, val[0], optionsDir);
      return val;
    });
  }
  return options;
}

function packageFor(type, name, optionsDir) {
  if (name.startsWith(".")) return path.resolve(optionsDir, name);
  if (name.startsWith("module:")) return name.slice("module:".length);
  if (path.isAbsolute(name)) return name;
  // Already a complete package name: an unscoped babel-plugin-*/babel-preset-*
  // (babel-plugin-polyfill-corejs3 and friends are published under those names,
  // not under @babel), or any other scope/subpath. Only the SHORT form gets the
  // @babel/plugin- prefix — this mirrors Babel's own standardizeName regexes.
  if (name.startsWith("babel-" + type + "-") || name.includes("/")) {
    const m = /^@babel\/(?:plugin-|preset-)?(.*)$/.exec(name);
    return m ? "@babel/" + type + "-" + m[1] : name;
  }
  return "@babel/" + type + "-" + name;
}

// collectTasks enumerates every task under one fixtures root.
// isGeneratorCorpus marks the one package whose fixtures are printer tests
// rather than transform tests.
let isGeneratorCorpus = false;

// checkoutRoot is the absolute guest path of the Babel checkout; expected
// output refers to it as "<CWD>".
let checkoutRoot = "/suite";

// normalizeOutput mirrors babel-helper-transform-fixture-test-runner's
// normalizeOutput: expected files were generated with the monorepo root
// replaced by the literal "<CWD>", and fixtures DO embed it — the development
// JSX transform writes the source path into __source.fileName, and
// transform-runtime's absoluteRuntime fixtures print resolved module paths.
function normalizeOutput(code) {
  return String(code).trim().split(checkoutRoot).join("<CWD>");
}

function collectTasks(fixturesRoot) {
  isGeneratorCorpus = fixturesRoot.includes("babel-generator");
  checkoutRoot = "/" + fixturesRoot.replace(/^\/+/, "").split("/")[0];
  // The fixtures root's own options.json is resolved too: a suite without its
  // own options.json inherits these, and an unresolved short name there would
  // reach Babel's config loader.
  const rootOpts = resolvePackages(readJSON(path.join(fixturesRoot, "options.json")) || {}, fixturesRoot);
  const tasks = [];
  for (const suiteName of fs.readdirSync(fixturesRoot).sort()) {
    if (suiteName.startsWith(".")) continue;
    const suiteDir = path.join(fixturesRoot, suiteName);
    if (!isDir(suiteDir)) continue;
    const suiteOpts = readJSON(path.join(suiteDir, "options.json"));
    // A suite's own options.json REPLACES the root's (Babel does not merge
    // them); without one it inherits the root's.
    const base = suiteOpts
      ? resolvePackages(suiteOpts, suiteDir)
      : JSON.parse(JSON.stringify(rootOpts));
    for (const taskName of fs.readdirSync(suiteDir).sort()) {
      if (taskName.startsWith(".")) continue;
      const taskDir = path.join(suiteDir, taskName);
      if (!isDir(taskDir)) continue;
      const options = JSON.parse(JSON.stringify(base));
      let optionsDir = suiteDir;
      const own = readJSON(path.join(taskDir, "options.json"));
      if (own) {
        Object.assign(options, resolvePackages(own, taskDir));
        optionsDir = taskDir;
      }
      tasks.push({ name: suiteName + "/" + taskName, dir: taskDir, options, optionsDir });
    }
  }
  return tasks;
}

// babelOptionsFor assembles what the fixture runner hands to @babel/core.
function babelOptionsFor(task, inputPath) {
  const opts = {
    cwd: task.dir,
    filename: inputPath,
    // Babel's runner reports the fixture-relative name "<suite>/<task>/<file>",
    // not the bare basename, and fixtures SEE it: transform-modules-* derives
    // the AMD/UMD module id from it and the development JSX transform embeds it
    // as __source.fileName.
    filenameRelative: task.name + "/" + path.basename(inputPath),
    sourceFileName: task.name + "/" + path.basename(inputPath),
    babelrc: false,
    configFile: false,
    browserslistConfigFile: false,
  };
  if (!task.options.DO_NOT_SET_SOURCE_TYPE) opts.sourceType = "script";
  for (const [k, v] of Object.entries(task.options)) {
    if (!HARNESS_KEYS.has(k)) opts[k] = v;
  }
  // Fixtures assume the external-helpers plugin unless they opt out; their
  // expected output references the `babelHelpers` global it installs. The
  // helperVersion is load-bearing and must match the one Babel's own runner
  // uses: it tells Babel which helpers the external bundle is new enough to
  // provide, and a lower value makes Babel INLINE the newer helpers instead —
  // which reads as an output mismatch in hundreds of class/private-field
  // fixtures.
  if (task.options.externalHelpers !== false) {
    opts.plugins = (opts.plugins || []).concat([
      ["@babel/plugin-external-helpers", { helperVersion: EXTERNAL_HELPERS_VERSION }],
    ]);
  }
  // Substitute the imported plugin/preset objects for their names. Babel would
  // resolve a name itself, but only by require()ing it first, and every Babel 8
  // plugin is an ES module — which this runtime cannot require() (Node >= 22
  // can). Passing the objects is a supported programmatic form and keeps the
  // fixture measuring the transform rather than the config loader.
  for (const key of ["plugins", "presets"]) {
    if (!Array.isArray(opts[key])) continue;
    opts[key] = opts[key].map((entry) => {
      const val = Array.isArray(entry) ? entry.slice() : [entry];
      if (typeof val[0] === "string") {
        if (!loaded.has(val[0])) throw new Error("could not load " + val[0] + ": " + (loadErrors.get(val[0]) || "not requested"));
        val[0] = loaded.get(val[0]);
      }
      return val;
    });
  }
  return opts;
}

// loaded maps a plugin/preset package name to its imported implementation;
// loadErrors records why one could not be imported.
const loaded = new Map();
const loadErrors = new Map();

// importPackages dynamic-imports every plugin and preset the collected tasks
// name, once per shard.
async function importPackages(tasks) {
  const names = new Set(["@babel/plugin-external-helpers"]);
  for (const task of tasks) {
    for (const key of ["plugins", "presets"]) {
      for (const entry of task.options[key] || []) {
        const name = Array.isArray(entry) ? entry[0] : entry;
        if (typeof name === "string") names.add(name);
      }
    }
  }
  for (const name of names) {
    try {
      const mod = await import(name.startsWith("/") ? name : name);
      loaded.set(name, mod.default ?? mod);
    } catch (e) {
      loadErrors.set(name, String((e && e.message) || e));
    }
  }
}

// runGeneratorTask implements babel-generator's fixture protocol
// (packages/babel-generator/test/index.js), which is NOT the transform one: its
// options.json holds PARSER plugins and generator options, and the fixture
// parses the input and prints it back rather than transforming it. Running
// those through the transform runner mis-reads "plugins": ["typescript"] as a
// Babel plugin package and fails every one of them.
function runGeneratorTask(task, inputPath) {
  const input = readFile(inputPath);
  const opts = task.options;
  const parserOpts = {
    filename: inputPath,
    plugins: opts.plugins || [],
    strictMode: opts.strictMode === false ? false : true,
    sourceType: "module",
    ...opts.parserOpts,
  };
  const genOpts = {};
  for (const [k, v] of Object.entries(opts)) {
    if (!HARNESS_KEYS.has(k) && k !== "plugins" && k !== "parserOpts") genOpts[k] = v;
  }
  let code, err;
  try {
    code = generate(parse(input, parserOpts), genOpts, input).code;
  } catch (e) {
    err = e;
  }
  if (opts.throws) {
    if (!err) return { status: "fail", reason: "expected a throw, generation succeeded" };
    if (opts.throws !== true && !String(err.message).includes(opts.throws)) {
      return { status: "fail", reason: "wrong error\n  want: " + opts.throws + "\n  got:  " + err.message };
    }
    return { status: "pass" };
  }
  if (err) return { status: "fail", reason: "threw: " + (err.message || String(err)) };
  const expectedPath = findFile(task.dir, "output");
  if (!expectedPath) return { status: "skip", reason: "no output file" };
  const want = readFile(expectedPath);
  const got = normalizeOutput(code);
  if (got !== want) {
    return { status: "fail", reason: "output mismatch\n--- want\n" + want + "\n--- got\n" + got };
  }
  return { status: "pass" };
}

// runTask returns {status, reason} for one fixture.
function runTask(task) {
  if (task.options.BABEL_8_BREAKING === false) {
    return { status: "skip", reason: "BABEL_8_BREAKING: false" };
  }
  if (task.options.os) {
    const list = [].concat(task.options.os);
    if (!list.includes(process.platform)) {
      return { status: "skip", reason: "os: " + list.join(",") };
    }
  }
  const inputPath = findFile(task.dir, "input");
  const execPath = findFile(task.dir, "exec");
  if (!inputPath) {
    // exec.js fixtures assert at run time instead of comparing output. They
    // need Babel's own test context (assert, a module registry); running them
    // is a separate mode, not a silent pass.
    return { status: "skip", reason: execPath ? "exec.js fixture" : "no input file" };
  }
  if (isGeneratorCorpus) return runGeneratorTask(task, inputPath);

  const expectedPath = findFile(task.dir, "output");
  const input = readFile(inputPath);
  const throwsExpected = task.options.throws;

  let result, err;
  try {
    result = babel.transformSync(input, babelOptionsFor(task, inputPath));
  } catch (e) {
    err = e;
  }

  if (throwsExpected) {
    if (!err) return { status: "fail", reason: "expected a throw, transform succeeded" };
    if (throwsExpected !== true && !String(err.message).includes(throwsExpected)) {
      return {
        status: "fail",
        reason: "wrong error\n  want: " + throwsExpected + "\n  got:  " + err.message,
      };
    }
    return { status: "pass" };
  }
  if (err) {
    return { status: "fail", reason: "threw: " + (err.message || String(err)) };
  }
  if (task.options.ignoreOutput) return { status: "pass" };
  if (!expectedPath) {
    // No output file: Babel's runner WRITES one. Here it just means the
    // fixture has nothing to compare against.
    return { status: "skip", reason: "no output file" };
  }
  const want = readFile(expectedPath);
  const got = normalizeOutput(result.code);
  if (got !== want) {
    return { status: "fail", reason: "output mismatch\n--- want\n" + want + "\n--- got\n" + got };
  }
  return { status: "pass" };
}

// The host sets __babeltest_root, evaluates this module (top-level await runs
// the whole shard) and reads __babeltest_result back. It is done this way round
// because importing the plugins needs await, and a host call cannot await.
const fixturesRoot = globalThis.__babeltest_root;
const out = [];
let tasks = [];
try {
  tasks = collectTasks(fixturesRoot);
  await importPackages(tasks);
} catch (e) {
  out.push({ name: "<collect>", status: "fail", reason: String((e && e.stack) || e) });
}
for (const task of tasks) {
  let r;
  try {
    r = runTask(task);
  } catch (e) {
    r = { status: "fail", reason: "harness error: " + ((e && e.stack) || e) };
  }
  out.push({ name: task.name, status: r.status, reason: r.reason || "" });
}
globalThis.__babeltest_result = JSON.stringify(out);
