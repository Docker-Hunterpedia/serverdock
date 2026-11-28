import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { MAX_REGEN_BYTES, finish, inputsHash, verifyGrowth, yearGrowth } from '../scripts/screens.mjs';

const sh = (cwd, ...args) => execFileSync('git', args, { cwd, encoding: 'utf8' });
function repo() {
	const dir = mkdtempSync(join(tmpdir(), 'screens-repo-'));
	sh(dir, 'init', '-q');
	sh(dir, 'config', 'user.email', 't@example.invalid');
	sh(dir, 'config', 'user.name', 't');
	sh(dir, 'config', 'commit.gpgsign', 'false');
	return dir;
}
function put(dir, path, content) {
	mkdirSync(dirname(join(dir, path)), { recursive: true });
	writeFileSync(join(dir, path), content);
}
const commit = (dir, msg = 'c') => (sh(dir, 'add', '-A'), sh(dir, 'commit', '-qm', msg), sh(dir, 'rev-parse', 'HEAD').trim());
const tmp = () => mkdtempSync(join(tmpdir(), 'screens-dir-'));

test('inputsHash changes with an input and ignores everything else', () => {
	const d = repo();
	try {
		put(d, 'web/src/a.svelte', 'a');
		put(d, 'docs/api/openapi.json', '{}');
		put(d, 'README.md', 'r');
		commit(d);
		const h = inputsHash(d);
		assert.match(h, /^[0-9a-f]{40}$/);
		put(d, 'README.md', 'changed');
		put(d, 'web/playwright.config.ts', 'not an input');
		put(d, 'internal/x.go', 'go');
		commit(d);
		assert.equal(inputsHash(d), h, 'files outside the inputs do not change the hash');
		for (const [path, body] of [['web/src/a.svelte', 'b'], ['docs/api/openapi.json', '{"a":1}'], ['web/e2e/screens.capture.ts', 'x'], ['web/package-lock.json', '{}']]) {
			const before = inputsHash(d);
			put(d, path, body);
			commit(d);
			assert.notEqual(inputsHash(d), before, path);
		}
	} finally { rmSync(d, { recursive: true, force: true }); }
});

const entries = [{ id: 'services', route: '(shell)/services', viewport: 'desktop', theme: 'dark', files: [{ name: 'services-dark-800.webp', width: 800, height: 500, bytes: 4 }] }];
function raw(files) {
	const d = tmp();
	for (const [n, c] of Object.entries(files)) writeFileSync(join(d, n), c);
	writeFileSync(join(d, 'entries.json'), JSON.stringify({ entries }));
	return d;
}
function committedSet(files, manifest) {
	const d = tmp();
	for (const [n, c] of Object.entries(files)) writeFileSync(join(d, n), c);
	if (manifest) writeFileSync(join(d, 'manifest.json'), JSON.stringify(manifest));
	return d;
}
const ENV = { RUN_ID: '1', RUNNER_IMAGE: 'img-1', CHROMIUM: 'Chromium 1', CAPTURED_AT: '2026-11-28T10:00:00Z' };
const man = (over = {}) => ({ inputs_hash: 'h1', captured_at: '2026-11-01T00:00:00Z', run_id: '9', runner_image: 'img-1', chromium: 'Chromium 1', growth: [{ captured_at: '2026-11-01T00:00:00Z', bytes_added: 100 }], entries, ...over });
const run = (r, c, over = {}) => {
	const out = tmp();
	try { return { ...finish({ raw: r, committed: c, out, hash: 'h1', env: ENV, ...over }), outFiles: readdirSync(out).sort(), out }; } finally { /* caller reads outFiles */ }
};

test('finish: no committed set is reported, and the artifact holds everything', () => {
	const r = run(raw({ 'services-dark-800.webp': 'new1' }), committedSet({}, null));
	assert.equal(r.stale, true);
	assert.deepEqual(r.outFiles, ['manifest.json', 'services-dark-800.webp']);
	assert.match(r.warnings.join(), /no committed set/);
});

test('finish: a stale hash gives the refresh artifact with only the changed files', () => {
	const c = committedSet({ 'services-dark-800.webp': 'same', 'x-dark-800.webp': 'old' }, man({ inputs_hash: 'OLD' }));
	const r = run(raw({ 'services-dark-800.webp': 'same', 'x-dark-800.webp': 'new!' }), c);
	assert.equal(r.stale, true);
	assert.deepEqual(r.changed, ['x-dark-800.webp']);
	assert.deepEqual(r.outFiles, ['manifest.json', 'x-dark-800.webp'], 'the identical file is left out');
	assert.equal(r.bytesAdded, 4);
	assert.match(r.warnings.join(), /screens are stale/);
	const m = JSON.parse(readFileSync(join(r.out, 'manifest.json'), 'utf8'));
	assert.equal(m.growth.at(-1).bytes_added, 4, 'bytes_added is computed from the changed files, never taken from the old manifest');
	assert.equal(m.growth.length, 2);
	assert.equal(m.inputs_hash, 'h1');
});

test('finish: a stale hash with identical bytes still refreshes the manifest (growth 0)', () => {
	const c = committedSet({ 'services-dark-800.webp': 'same' }, man({ inputs_hash: 'OLD' }));
	const r = run(raw({ 'services-dark-800.webp': 'same' }), c);
	assert.deepEqual(r.outFiles, ['manifest.json']);
	assert.equal(r.manifest.growth.at(-1).bytes_added, 0);
});

test('finish: fresh and identical gives an empty artifact', () => {
	const c = committedSet({ 'services-dark-800.webp': 'same' }, man());
	const r = run(raw({ 'services-dark-800.webp': 'same' }), c);
	assert.equal(r.stale, false);
	assert.deepEqual(r.outFiles, []);
	assert.deepEqual([r.warnings, r.problems], [[], []]);
});

test('finish: same hash but different bytes is a warning, and a problem on main', () => {
	const c = committedSet({ 'services-dark-800.webp': 'old!' }, man());
	let r = run(raw({ 'services-dark-800.webp': 'new!' }), c);
	assert.match(r.warnings.join(), /unchanged hash/);
	assert.deepEqual(r.problems, []);
	r = run(raw({ 'services-dark-800.webp': 'new!' }), c, { main: true });
	assert.match(r.problems.join(), /unchanged hash/);
});

test('finish: a changed runner image or Chromium is named as the cause', () => {
	const c = committedSet({ 'services-dark-800.webp': 'old!' }, man({ chromium: 'Chromium 0' }));
	const r = run(raw({ 'services-dark-800.webp': 'new!' }), c);
	assert.match(r.warnings.join(), /runner image or Chromium changed/);
});

test('finish: a file no longer produced is reported', () => {
	const c = committedSet({ 'services-dark-800.webp': 'same', 'gone-dark-800.webp': 'x' }, man());
	const r = run(raw({ 'services-dark-800.webp': 'same' }), c);
	assert.deepEqual(r.removed, ['gone-dark-800.webp']);
	assert.match(r.warnings.join(), /git rm/);
});

test('finish: the growth caps fail the job (2 MB per regeneration, 50 MB per 365 days)', () => {
	const big = 'x'.repeat(MAX_REGEN_BYTES + 1);
	let r = run(raw({ 'services-dark-800.webp': big }), committedSet({}, man({ inputs_hash: 'OLD' })));
	assert.match(r.problems.join(), /one regeneration adds/);
	const heavy = man({ inputs_hash: 'OLD', growth: Array.from({ length: 27 }, () => ({ captured_at: '2026-10-01T00:00:00Z', bytes_added: 1_900_000 })) });
	r = run(raw({ 'services-dark-800.webp': 'new!' }), committedSet({ 'services-dark-800.webp': 'old!' }, heavy));
	assert.match(r.problems.join(), /in 365 days/);
	// old entries fall out of the window
	assert.equal(yearGrowth([{ captured_at: '2025-01-01T00:00:00Z', bytes_added: 99 }, { captured_at: '2026-11-01T00:00:00Z', bytes_added: 1 }], Date.parse('2026-11-28T00:00:00Z')), 1);
});

test('verifyGrowth recomputes the last entry from git history', () => {
	const d = repo();
	try {
		const dir = 'site/public/screens/';
		put(d, dir + 'a-dark-800.webp', 'x'.repeat(10));
		put(d, dir + 'b-dark-800.webp', 'y'.repeat(7));
		put(d, dir + 'manifest.json', JSON.stringify({ growth: [{ captured_at: '2026-11-28T00:00:00Z', bytes_added: 17 }] }));
		commit(d, 'first set');
		assert.equal(verifyGrowth(d).ok, true);
		put(d, dir + 'a-dark-800.webp', 'z'.repeat(12)); // changed: 12 new bytes
		put(d, dir + 'manifest.json', JSON.stringify({ growth: [{ captured_at: '2026-11-28T00:00:00Z', bytes_added: 17 }, { captured_at: '2026-11-29T00:00:00Z', bytes_added: 99 }] }));
		commit(d, 'refresh');
		const r = verifyGrowth(d);
		assert.equal(r.ok, false);
		assert.match(r.message, /says 99 bytes.*added 12/);
	} finally { rmSync(d, { recursive: true, force: true }); }
});
