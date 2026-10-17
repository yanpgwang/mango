const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { test } = require('node:test');
const { Manifest, setLogger, VERSION: upstreamVersion } = require('release-please');

const root = path.resolve(__dirname, '../../..');
const current = fs.readFileSync(path.join(root, 'VERSION'), 'utf8').trim();
const next = `0.1.0-alpha.${Number(current.match(/^0\.1\.0-alpha\.(\d+)$/)[1]) + 1}`;
const baseline = '1'.repeat(40);
const python = process.env.MANGO_RELEASE_TEST_PYTHON || 'python3';
const logger = Object.fromEntries(['debug', 'info', 'warn', 'error', 'trace'].map(key => [key, () => {}]));
setLogger(logger);

function github(message, overrides = {}, source = root) {
  return {
    repository: { owner: 'yanpgwang', repo: 'mango' },
    async getFileContentsOnBranch(file) {
      return { parsedContent: fs.readFileSync(path.join(source, file), 'utf8') };
    },
    async getFileJson(file) {
      return JSON.parse((await this.getFileContentsOnBranch(file)).parsedContent);
    },
    async *releaseIterator() {
      yield { tagName: `v${fs.readFileSync(path.join(source, 'VERSION'), 'utf8').trim()}`, sha: baseline, notes: 'Existing public preview' };
    },
    async *mergeCommitIterator() {
      yield { sha: '2'.repeat(40), message, files: ['internal/example.go'] };
      yield { sha: baseline, message: 'previous public release', files: [] };
    },
    async *pullRequestIterator() {},
    ...overrides,
  };
}

async function candidate(message, source = root) {
  const version = fs.readFileSync(path.join(source, 'VERSION'), 'utf8').trim();
  const expected = `0.1.0-alpha.${Number(version.match(/^0\.1\.0-alpha\.(\d+)$/)[1]) + 1}`;
  const manifest = await Manifest.fromManifest(github(message, {}, source), 'main');
  const prs = await manifest.buildPullRequests();
  assert.equal(prs.length, 1);
  assert.equal(prs[0].title.toString(), `chore(main): release ${expected}`);
  assert.equal(prs[0].headRefName, 'release-please--branches--main');
  return prs[0];
}

test('test dependency matches the official action bundle', () => {
  assert.equal(upstreamVersion, '17.6.0');
});

for (const message of ['fix: correct a runtime failure', 'feat: add a user workflow', 'feat!: revise the development API']) {
  test(`native manifest/updaters produce a matched canonical alpha: ${message}`, async () => {
    const pr = await candidate(message);
    const files = {};
    for (const update of pr.updates) {
      const filename = path.join(root, update.path);
      const before = fs.existsSync(filename) ? fs.readFileSync(filename, 'utf8') : undefined;
      files[update.path] = update.updater.updateContent(before, logger);
    }
    assert.equal(files.VERSION.trim(), next);
    assert.deepEqual(JSON.parse(files['.release-please-manifest.json']), { '.': next });
    assert.match(files['CHANGELOG.md'], new RegExp(next.replaceAll('.', '\\.')));
    assert.ok(files['CHANGELOG.md'].includes(message.split(': ')[1]));
    assert.equal(JSON.parse(files['sdk/typescript/package.json']).version, next);
    const lock = JSON.parse(files['sdk/typescript/package-lock.json']);
    assert.equal(lock.version, next);
    assert.equal(lock.packages[''].version, next);
    const oldLock = JSON.parse(fs.readFileSync(path.join(root, 'sdk/typescript/package-lock.json'), 'utf8'));
    delete lock.version; delete oldLock.version;
    delete lock.packages[''].version; delete oldLock.packages[''].version;
    assert.deepEqual(lock, oldLock, 'SDK dependency lock entries must remain unchanged');

    const pythonFiles = JSON.parse(execFileSync(python, ['-c',
      `import json,sys; from pathlib import Path; sys.path.insert(0,${JSON.stringify(path.join(root, 'scripts/release'))}); from please import PYTHON_FILES; print(json.dumps({p:(Path(${JSON.stringify(root)})/p).read_text() for p in PYTHON_FILES}))`
    ], { encoding: 'utf8' }));
    const mapped = JSON.parse(execFileSync(python, ['-c',
      `import json,sys; sys.path.insert(0,${JSON.stringify(path.join(root, 'scripts/release'))}); from please import python_updates; print(json.dumps(python_updates(json.load(sys.stdin),${JSON.stringify(current)},${JSON.stringify(next)})))`
    ], { input: JSON.stringify({ ...pythonFiles, ...files }), encoding: 'utf8' }));
    Object.assign(files, { ...pythonFiles, ...files }, mapped);
    const targetPython = next.replace('-alpha.', 'a');
    assert.ok(files['sdk/python/pyproject.toml'].includes(`version = "${targetPython}"`));
    assert.ok(files['sdk/python/src/mango_sdk/_version.py'].includes(`__version__ = "${targetPython}"`));
    assert.ok(files['sdk/python/uv.lock'].includes(`name = "mango-sdk"\nversion = "${targetPython}"`));
    for (const [filename, content] of Object.entries(files)) {
      if (filename.endsWith('.md') && filename !== 'CHANGELOG.md') {
        assert.ok(!content.includes(current), `stale current SemVer in ${filename}`);
        assert.ok(!content.includes(current.replace('-alpha.', 'a')), `stale Python version in ${filename}`);
        assert.ok(!content.includes(next + 'a'), `generic updater corrupted Python in ${filename}`);
      }
    }
    const temporary = fs.mkdtempSync(path.join(os.tmpdir(), 'mango-bot-native-'));
    try {
      const source = path.join(temporary, 'source');
      fs.mkdirSync(source);
      for (const [filename, content] of Object.entries(files)) {
        fs.mkdirSync(path.dirname(path.join(source, filename)), { recursive: true });
        fs.writeFileSync(path.join(source, filename), content);
      }
      execFileSync('git', ['init', '-q', source]);
      execFileSync('git', ['-C', source, 'add', '.']);
      execFileSync('git', ['-C', source, '-c', 'user.name=Release test', '-c', 'user.email=release@example.invalid', 'commit', '-qm', 'native version candidate']);
      execFileSync(python, ['-c',
        `import sys; from pathlib import Path; sys.path.insert(0,${JSON.stringify(path.join(root, 'scripts/release'))}); from build import validate_source; validate_source(Path(${JSON.stringify(source)}),${JSON.stringify(next)},Path(${JSON.stringify(path.join(temporary, 'candidate'))}))`
      ]);
    } finally {
      fs.rmSync(temporary, { recursive: true, force: true });
    }
  });
}

test('native merged-pending mutex holds even when a partial publication already has a tag', async () => {
  const previousPR = await candidate('fix: prepare a reviewed preview');
  const merged = { number: 42, title: previousPR.title.toString(), body: previousPR.body.toString(),
    headBranchName: previousPR.headRefName, labels: ['autorelease: pending'], sha: baseline };
  const api = github('feat: later work must wait', {
    async getFileContentsOnBranch(file) {
      return { parsedContent: file === '.release-please-manifest.json'
        ? JSON.stringify({ '.': next }) : fs.readFileSync(path.join(root, file), 'utf8') };
    },
    async *tagIterator() { yield { name: `v${next}`, sha: baseline }; },
    async *pullRequestIterator(branch, state) { if (state === 'MERGED') yield merged; },
    async createPullRequest() { assert.fail('a pending publication must not open another version PR'); },
    async createRelease() { assert.fail('release-please must never publish'); },
  });
  const manifest = await Manifest.fromManifest(api, 'main');
  assert.deepEqual(await manifest.createPullRequests(), []);
  assert.deepEqual(await manifest.createReleases(), [], 'skip-github-release config is a second publication guard');
});

test('the complete release unit suite passes on two successive native-generated version trees', async () => {
  const temporary = fs.mkdtempSync(path.join(os.tmpdir(), 'mango-bot-future-'));
  const source = path.join(temporary, 'source');
  const config = JSON.parse(fs.readFileSync(path.join(root, 'release-please-config.json'), 'utf8'));
  const pythonPaths = JSON.parse(execFileSync(python, ['-c',
    `import json,sys; sys.path.insert(0,${JSON.stringify(path.join(root, 'scripts/release'))}); from please import PYTHON_FILES; print(json.dumps(PYTHON_FILES))`
  ], { encoding: 'utf8' }));
  const paths = new Set(['VERSION', '.release-please-manifest.json', 'release-please-config.json', ...pythonPaths,
    ...config.packages['.']['extra-files'].map(file => typeof file === 'string' ? file : file.path),
    ...fs.readdirSync(path.join(root, 'scripts/release')).filter(file => file.endsWith('.py')).map(file => `scripts/release/${file}`)]);
  if (fs.existsSync(path.join(root, 'CHANGELOG.md'))) paths.add('CHANGELOG.md');
  try {
    for (const filename of paths) {
      fs.mkdirSync(path.dirname(path.join(source, filename)), { recursive: true });
      fs.copyFileSync(path.join(root, filename), path.join(source, filename));
    }
    for (let generation = 0; generation < 2; generation++) {
      const version = fs.readFileSync(path.join(source, 'VERSION'), 'utf8').trim();
      const pr = await candidate('fix: validate the next matched source snapshot', source);
      for (const update of pr.updates) {
        const filename = path.join(source, update.path);
        const before = fs.existsSync(filename) ? fs.readFileSync(filename, 'utf8') : undefined;
        fs.mkdirSync(path.dirname(filename), { recursive: true });
        fs.writeFileSync(filename, update.updater.updateContent(before, logger));
      }
      const target = fs.readFileSync(path.join(source, 'VERSION'), 'utf8').trim();
      const contents = Object.fromEntries(pythonPaths.map(filename => [filename, fs.readFileSync(path.join(source, filename), 'utf8')]));
      const changes = JSON.parse(execFileSync(python, ['-c',
        `import json,sys; sys.path.insert(0,${JSON.stringify(path.join(root, 'scripts/release'))}); from please import python_updates; print(json.dumps(python_updates(json.load(sys.stdin),${JSON.stringify(version)},${JSON.stringify(target)})))`
      ], { input: JSON.stringify(contents), encoding: 'utf8' }));
      for (const [filename, content] of Object.entries(changes)) fs.writeFileSync(path.join(source, filename), content);
      // This executes the actual suite from the generated source, so ROOT and
      // VERSION advance together. No test class or expected result is mocked.
      execFileSync(python, ['-m', 'unittest', 'discover', '-s', 'scripts/release', '-p', 'test_*.py'],
        { cwd: source, stdio: 'pipe' });
      console.log(`Complete release unit suite passed on native source ${target}`);
    }
  } finally {
    fs.rmSync(temporary, { recursive: true, force: true });
  }
});
