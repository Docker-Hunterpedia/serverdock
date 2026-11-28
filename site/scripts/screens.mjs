// The freshness and refresh logic of the screenshots (plan 0014, Revision 3). node: built-ins only,
// so the `screens` jobs in ci.yml run it with no install.
//
//   node scripts/screens.mjs hash                       print the inputs hash (40 hex)
//   node scripts/screens.mjs state [manifest.json]      print stale=true|false (stale when the manifest is
//                                                       missing or its inputs_hash differs from the hash)
//   node scripts/screens.mjs finish --raw <dir> --committed <dir> --out <dir> [--main] [--strict]
//                                                       compare a fresh capture with the committed set and write
//                                                       the refresh artifact (only changed files and the manifest)
//   node scripts/screens.mjs verify-growth [manifest.json]
//                                                       recompute the last growth entry from git history
//
// finish reads RUN_ID, RUNNER_IMAGE and CHROMIUM from the environment (CAPTURED_AT overrides the clock, for tests).
import { execFileSync } from 'node:child_process';
import { appendFileSync, copyFileSync, existsSync, mkdirSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

// What the screenshots depend on (plan table). The hash covers exactly these paths, as committed.
export const INPUTS = [
	'web/src',
	'web/e2e/screens.capture.ts',
	'web/e2e/seed.ts',
	'web/e2e/server.ts',
	'web/e2e/global-setup.ts',
	'web/e2e/fixtures.ts', // the capture imports it
	'web/playwright.screens.config.ts',
	'web/package-lock.json',
	'web/vite.config.ts',
	'web/svelte.config.js', // neither exists today
	'web/static',
	'docs/api/openapi.json',
];
// One regeneration. In practice this cannot trigger: the 24 files at their per-file caps come to 1.87 MB, so the
// effective limit is the 50 MB a year below (about 27 full regenerations).
export const MAX_REGEN_BYTES = 2_000_000;
export const MAX_YEAR_BYTES = 50_000_000; // the sum of `growth` over 365 days
const DAY = 86400000;

const git = (args, cwd, input) => execFileSync('git', args, { cwd, input, encoding: 'utf8', maxBuffer: 64 << 20 });

/** The hash of the inputs: the git blob ids and paths of every input file at `ref`, hashed once more. */
export function inputsHash(repo = '.', ref = 'HEAD') {
	const listing = git(['ls-tree', '-r', ref, '--', ...INPUTS], repo);
	return git(['hash-object', '--stdin'], repo, listing).trim();
}

export function readManifest(path) {
	return existsSync(path) ? JSON.parse(readFileSync(path, 'utf8')) : null;
}

const files = (dir) => (existsSync(dir) ? readdirSync(dir).filter((n) => n.endsWith('.webp')).sort() : []);
const same = (a, b) => existsSync(a) && existsSync(b) && readFileSync(a).equals(readFileSync(b));

/** Sum of bytes_added over the entries of the last 365 days before `now` (inclusive of entries dated `now`). */
export function yearGrowth(growth, now) {
	return growth.filter((g) => now - Date.parse(g.captured_at) <= 365 * DAY).reduce((s, g) => s + g.bytes_added, 0);
}

/**
 * Compares a fresh capture (`raw`: the webp files and entries.json) with the committed set and writes the refresh
 * artifact into `out`: changed files and the new manifest only. Returns what happened, never throws on a stale set.
 */
export function finish({ raw, committed, out, hash, env = {}, main = false, now = Date.now() }) {
	const captured = readManifest(join(committed, 'manifest.json'));
	const { entries } = JSON.parse(readFileSync(join(raw, 'entries.json'), 'utf8'));
	const names = files(raw);
	const changed = names.filter((n) => !same(join(raw, n), join(committed, n)));
	const removed = files(committed).filter((n) => !names.includes(n));
	const bytesAdded = changed.reduce((s, n) => s + statSync(join(raw, n)).size, 0);
	const hashMatches = captured?.inputs_hash === hash;
	const problems = [];
	const warnings = [];
	const capturedAt = env.CAPTURED_AT ?? new Date(now).toISOString().replace(/\.\d{3}Z$/, 'Z');

	const growth = [...(captured?.growth ?? [])];
	if (!hashMatches || changed.length > 0) growth.push({ captured_at: capturedAt, bytes_added: bytesAdded });
	if (bytesAdded > MAX_REGEN_BYTES) problems.push(`one regeneration adds ${bytesAdded} bytes, over ${MAX_REGEN_BYTES}: switch to "regenerate at milestone close"`);
	const year = yearGrowth(growth, Date.parse(capturedAt));
	if (year > MAX_YEAR_BYTES) problems.push(`the screenshots grew by ${year} bytes in 365 days, over ${MAX_YEAR_BYTES}: switch to "regenerate at milestone close"`);

	if (!captured) warnings.push('there is no committed set yet: commit the artifact to create it');
	else if (!hashMatches) warnings.push('screens are stale: run the refresh procedure (CONTRIBUTING.md, "Refresh screenshots")');
	else if (changed.length > 0) {
		const cause = `screens differ with an unchanged hash (${changed.join(', ')}): an input is missing from the hash, or rendering is not deterministic`;
		if (main) problems.push(cause);
		else warnings.push(cause);
		if (captured.runner_image !== env.RUNNER_IMAGE || captured.chromium !== env.CHROMIUM) {
			warnings.push(`the runner image or Chromium changed (committed: ${captured.runner_image}, ${captured.chromium}; now: ${env.RUNNER_IMAGE}, ${env.CHROMIUM}): the likely cause`);
		}
	}
	if (removed.length) warnings.push(`committed files no longer produced (remove them with git rm): ${removed.join(', ')}`);

	const manifest = {
		inputs_hash: hash,
		captured_at: capturedAt,
		run_id: String(env.RUN_ID ?? ''),
		runner_image: env.RUNNER_IMAGE ?? '',
		chromium: env.CHROMIUM ?? '',
		growth,
		entries,
	};
	rmSync(out, { recursive: true, force: true });
	mkdirSync(out, { recursive: true });
	const refresh = !hashMatches || changed.length > 0;
	if (refresh) {
		for (const n of changed) copyFileSync(join(raw, n), join(out, n));
		writeFileSync(join(out, 'manifest.json'), JSON.stringify(manifest, null, '\t') + '\n');
	}
	return { stale: !hashMatches, hashMatches, changed, removed, bytesAdded, refresh, problems, warnings, manifest };
}

const readAt = (repo, rev, path) => {
	try {
		return JSON.parse(git(['show', `${rev}:${path}`], repo));
	} catch {
		return null;
	}
};

/** Bytes of the .webp files added or changed between `from` and `commit` under `dir` (`from` null: the root commit). */
function addedBytes(repo, from, commit, dir) {
	const args = from ? ['diff-tree', '-r', '--no-commit-id', from, commit, '--', dir] : ['diff-tree', '-r', '--no-commit-id', '--root', commit, '--', dir];
	let added = 0;
	for (const line of git(args, repo).split('\n').filter(Boolean)) {
		const [meta, path] = line.split('\t');
		const [, , , newBlob, status] = meta.split(' ');
		if (!path.endsWith('.webp') || status === 'D') continue;
		added += Number(git(['cat-file', '-s', newBlob], repo).trim());
	}
	return added;
}

/**
 * The last growth entry must equal the bytes the last commit that touched the manifest added or changed. For a
 * merge commit the comparison is against the parent whose growth list is one entry shorter; if no parent fits (a
 * hand-resolved manifest, or a merge of several refreshes) the result is a warning, never a failure. A merge whose numbers
 * differ also only warns; an ordinary commit that differs fails.
 */
export function verifyGrowth(repo = '.', manifestPath = 'site/public/screens/manifest.json') {
	const m = readManifest(resolve(repo, manifestPath));
	if (!m || m.growth.length === 0) return { ok: true, message: 'no growth entries to verify' };
	const commit = git(['log', '-1', '--first-parent', '--format=%H', '--', manifestPath], repo).trim();
	if (!commit) return { ok: false, message: 'the manifest is not in git history' };
	const dir = manifestPath.replace(/manifest\.json$/, '');
	const parents = git(['rev-list', '--parents', '-n', '1', commit], repo).trim().split(' ').slice(1);
	const last = m.growth[m.growth.length - 1].bytes_added;
	let from = null;
	if (parents.length === 1) from = parents[0];
	else if (parents.length > 1) {
		from = parents.find((p) => (readAt(repo, p, manifestPath)?.growth?.length ?? 0) === m.growth.length - 1) ?? undefined;
		if (from === undefined) return { ok: true, warning: true, message: `cannot verify the last growth entry: ${commit.slice(0, 8)} is a merge and no parent has one growth entry fewer` };
	}
	const added = addedBytes(repo, from, commit, dir);
	if (last === added) return { ok: true, message: `last growth entry matches: ${added} bytes` };
	const text = `the last growth entry says ${last} bytes, commit ${commit.slice(0, 8)} added ${added}`;
	// a merge can legitimately differ (a hand-resolved manifest): warn there, fail only on an ordinary commit
	return parents.length > 1 ? { ok: true, warning: true, message: `cannot verify the last growth entry of a merge: ${text}` } : { ok: false, message: text };
}

function arg(args, name) {
	const i = args.indexOf(name);
	return i >= 0 ? args[i + 1] : undefined;
}
const summary = (text) => {
	console.log(text);
	if (process.env.GITHUB_STEP_SUMMARY) appendFileSync(process.env.GITHUB_STEP_SUMMARY, text + '\n');
};

if (process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1])) {
	const [cmd, ...args] = process.argv.slice(2);
	const repoRoot = resolve(fileURLToPath(new URL('../..', import.meta.url)));
	if (cmd === 'hash') {
		console.log(inputsHash(repoRoot));
	} else if (cmd === 'state') {
		const path = args[0] ?? join(repoRoot, 'site/public/screens/manifest.json');
		const m = readManifest(path);
		const stale = !m || m.inputs_hash !== inputsHash(repoRoot);
		console.log(`stale=${stale}`);
		if (process.env.GITHUB_OUTPUT) appendFileSync(process.env.GITHUB_OUTPUT, `stale=${stale}\n`);
		summary(stale ? '### Screenshots\n\nThe committed screenshots do not match the current inputs (stale).' : '### Screenshots\n\nThe committed screenshots match the current inputs (fresh).');
	} else if (cmd === 'finish') {
		const raw = arg(args, '--raw'), committed = arg(args, '--committed'), out = arg(args, '--out');
		if (!raw || !committed || !out) {
			console.error('usage: screens.mjs finish --raw <dir> --committed <dir> --out <dir> [--main] [--strict]');
			process.exit(2);
		}
		for (const v of ['RUNNER_IMAGE', 'CHROMIUM']) {
			if (!/^[A-Za-z0-9._ -]{1,64}$/.test(process.env[v] ?? '')) {
				console.error(`${v} must match ^[A-Za-z0-9._ -]{1,64}$`);
				process.exit(2);
			}
		}
		if (!/^\d+$/.test(process.env.RUN_ID ?? '')) {
			console.error('RUN_ID must be digits');
			process.exit(2);
		}
		const r = finish({ raw: resolve(raw), committed: resolve(committed), out: resolve(out), hash: inputsHash(repoRoot), env: process.env, main: args.includes('--main') });
		const lines = ['### Screenshots', '', `- inputs hash: ${r.manifest.inputs_hash} (${r.hashMatches ? 'matches the committed manifest' : 'differs from the committed manifest'})`, `- files changed: ${r.changed.length} (${r.bytesAdded} bytes)`];
		for (const w of r.warnings) lines.push(`- WARNING: ${w}`);
		for (const p of r.problems) lines.push(`- PROBLEM: ${p}`);
		if (r.refresh) lines.push('- the refresh artifact `site-screens` holds the changed files and the new manifest');
		summary(lines.join('\n'));
		// stale or differing sets fail the job (it is a warning while the job has continue-on-error); growth problems always fail
		process.exit(r.problems.length || (r.stale && !args.includes('--allow-stale')) || r.changed.length ? 1 : 0);
	} else if (cmd === 'verify-growth') {
		const r = verifyGrowth(repoRoot, args[0] ?? 'site/public/screens/manifest.json');
		summary(`### Screenshot growth\n\n${r.warning ? 'WARNING: ' : ''}${r.message}`);
		process.exit(r.ok ? 0 : 1);
	} else {
		console.error('usage: screens.mjs hash | state | finish | verify-growth');
		process.exit(2);
	}
}
