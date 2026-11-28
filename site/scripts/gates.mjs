// Build gates 1-7 of plan 0014 section 3.5. Only node: built-ins are imported, so
// this runs with no node_modules (the deploy job runs it before the token step).
//
//   node scripts/gates.mjs [--dist-only] <dist>
//
// --dist-only runs gates 1-5 (what the deploy job re-checks). Without it, gates
// 6 and 7 also run. Exit 0 when every gate passes, 1 otherwise. Output names
// files and gates only, never file contents.
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { join, posix, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

const DAY = 86400000;
const MIN_EXPIRY_DAYS = 30;

function walk(dir) {
	if (!existsSync(dir)) return [];
	return readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
		const p = join(dir, e.name);
		return e.isDirectory() ? walk(p) : [p];
	});
}
const rel = (dist, p) => relative(dist, p).split(sep).join('/');
const read = (p) => readFileSync(p, 'utf8');

// A start tag, allowing ">" inside quoted attribute values.
const TAG = /<([a-zA-Z][^\s/>]*)((?:"[^"]*"|'[^']*'|[^'">])*)>/g;

/** Gate 1: no <script>, <style>, on*= or style= in any HTML or SVG file. */
export function gateNoInlineCode(dist) {
	const errs = [];
	for (const f of walk(dist)) {
		if (!/\.(html|svg)$/i.test(f)) continue;
		const src = read(f);
		for (const m of src.matchAll(TAG)) {
			const name = m[1].toLowerCase();
			if (name === 'script') errs.push(`${rel(dist, f)}: <script> element`);
			if (name === 'style') errs.push(`${rel(dist, f)}: <style> element`);
			if (/\s(on[a-z]+)\s*=/i.test(' ' + m[2])) errs.push(`${rel(dist, f)}: on*= attribute on <${name}>`);
			if (/\sstyle\s*=/i.test(' ' + m[2])) errs.push(`${rel(dist, f)}: style= attribute on <${name}>`);
		}
	}
	return errs;
}

/** Gate 2: _headers exists and forbids scripts. */
export function gateHeaders(dist) {
	const p = join(dist, '_headers');
	if (!existsSync(p)) return ['_headers is missing'];
	if (!read(p).includes("script-src 'none'")) return ["_headers has no script-src 'none'"];
	return [];
}

/** Gate 3: security.txt exists and Expires is at least 30 days away. */
export function gateSecurityTxt(dist, now = Date.now()) {
	const p = join(dist, '.well-known', 'security.txt');
	if (!existsSync(p)) return ['.well-known/security.txt is missing'];
	const m = /^Expires:[ \t]*(\S+)[ \t]*$/im.exec(read(p));
	if (!m) return ['security.txt has no Expires field'];
	const t = Date.parse(m[1]);
	if (Number.isNaN(t)) return ['security.txt Expires is not a date'];
	if (t - now < MIN_EXPIRY_DAYS * DAY) return [`security.txt Expires is less than ${MIN_EXPIRY_DAYS} days away`];
	return [];
}

/** Gate 4: no Pages Functions or worker output. */
export function gateNoFunctions(dist) {
	return ['_worker.js', 'functions', '_routes.json'].filter((n) => existsSync(join(dist, n))).map((n) => `${n} must not exist`);
}

/** Gate 5: no /styleguide/ in a production build. */
export function gateNoStyleguide(dist) {
	return existsSync(join(dist, 'styleguide')) ? ['styleguide/ must not exist in a production build'] : [];
}

/** Gate 6: the dist/docs/<slug>/ routes equal the expected set. */
export function gateDocsRoutes(dist, expected) {
	const base = join(dist, 'docs');
	const got = existsSync(base)
		? readdirSync(base, { withFileTypes: true })
				.filter((e) => e.isDirectory() && existsSync(join(base, e.name, 'index.html')))
				.map((e) => e.name)
		: [];
	const want = new Set(expected);
	const have = new Set(got);
	return [
		...got.filter((s) => !want.has(s)).map((s) => `docs/${s}/ is published but not in the allowlist`),
		...expected.filter((s) => !have.has(s)).map((s) => `docs/${s}/ is in the allowlist but not built`),
	];
}

const ATTR = /\s(href|src)\s*=\s*(?:"([^"]*)"|'([^']*)')/gi;
const ID = /\s(?:id|name)\s*=\s*(?:"([^"]*)"|'([^']*)')/gi;

/** Gate 7: every same-site href/src resolves to a file, and every #id exists. */
export function gateLinks(dist) {
	const errs = [];
	const root = resolve(dist);
	const pages = walk(dist).filter((f) => f.endsWith('.html'));
	const ids = new Map();
	const idsOf = (file) => {
		if (!ids.has(file)) {
			const set = new Set();
			for (const m of read(file).matchAll(ID)) set.add(m[1] ?? m[2]);
			ids.set(file, set);
		}
		return ids.get(file);
	};
	for (const page of pages) {
		const pageUrl = '/' + rel(dist, page);
		const src = read(page);
		for (const tag of src.matchAll(TAG)) {
			for (const m of tag[2].matchAll(ATTR)) {
				const raw = (m[2] ?? m[3]).trim();
				if (raw === '' || /^[a-zA-Z][a-zA-Z0-9+.-]*:/.test(raw) || raw.startsWith('//')) continue; // other scheme or host
				const hashAt = raw.indexOf('#');
				const pathPart = (hashAt < 0 ? raw : raw.slice(0, hashAt)).split('?')[0];
				let frag = hashAt < 0 ? '' : raw.slice(hashAt + 1);
				let target;
				try {
					target = pathPart === '' ? pageUrl : posix.resolve(posix.dirname(pageUrl), decodeURIComponent(pathPart));
					frag = decodeURIComponent(frag);
				} catch {
					errs.push(`${rel(dist, page)}: bad URL escape in ${m[1]}`);
					continue;
				}
				let file = join(root, target);
				if (!resolve(file).startsWith(root + sep) && resolve(file) !== root) {
					errs.push(`${rel(dist, page)}: ${m[1]}="${raw}" leaves the site`);
					continue;
				}
				if (pathPart.endsWith('/') || (existsSync(file) && statSync(file).isDirectory())) file = join(file, 'index.html');
				if (!existsSync(file) || !statSync(file).isFile()) {
					errs.push(`${rel(dist, page)}: ${m[1]}="${raw}" does not resolve`);
					continue;
				}
				if (frag !== '' && file.endsWith('.html') && !idsOf(file).has(frag)) {
					errs.push(`${rel(dist, page)}: ${m[1]}="${raw}" has no #${frag}`);
				}
			}
		}
	}
	return errs;
}

/** Run gates; returns { name, errs }[] . `expectedDocs` is the allowlist of doc slugs. */
export function runGates(dist, { distOnly = false, now = Date.now(), expectedDocs = [] } = {}) {
	const out = [
		['1 no inline code', gateNoInlineCode(dist)],
		['2 _headers', gateHeaders(dist)],
		['3 security.txt', gateSecurityTxt(dist, now)],
		['4 no functions', gateNoFunctions(dist)],
		['5 no styleguide', gateNoStyleguide(dist)],
	];
	if (!distOnly) {
		out.push(['6 docs routes', gateDocsRoutes(dist, expectedDocs)]);
		out.push(['7 links', gateLinks(dist)]);
	}
	return out.map(([name, errs]) => ({ name, errs }));
}

if (process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1])) {
	const args = process.argv.slice(2);
	const distOnly = args.includes('--dist-only');
	const paths = args.filter((a) => !a.startsWith('--'));
	if (paths.length !== 1) {
		console.error('usage: gates.mjs [--dist-only] <dist>');
		process.exit(2);
	}
	const dist = resolve(paths[0]);
	if (!existsSync(dist) || !statSync(dist).isDirectory()) {
		console.error(`gates: ${paths[0]} is not a directory`);
		process.exit(2);
	}
	// Step 3 supplies the real allowlist (site/src/docs.config.ts). Until then it is empty,
	// and the file's arrival without gate support fails closed.
	const cfg = fileURLToPath(new URL('../src/docs.config.ts', import.meta.url));
	if (!distOnly && existsSync(cfg)) {
		console.error('gates: src/docs.config.ts exists but gate 6 cannot expand it yet; wire the allowlist into gates.mjs');
		process.exit(1);
	}
	let failed = 0;
	for (const g of runGates(dist, { distOnly })) {
		if (g.errs.length === 0) console.log(`ok   gate ${g.name}`);
		else {
			failed++;
			console.log(`FAIL gate ${g.name}`);
			for (const e of g.errs) console.log(`       ${e}`);
		}
	}
	process.exit(failed ? 1 : 0);
}
