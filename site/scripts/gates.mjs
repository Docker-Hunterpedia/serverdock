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

const attrOf = (attrs, name) => {
	const m = new RegExp(`(?:^|[\\s"'/])${name}\\s*=\\s*(?:"([^"]*)"|'([^']*)'|([^\\s"'>]+))`, 'i').exec(attrs);
	return m ? (m[1] ?? m[2] ?? m[3]) : null;
};

/**
 * Gate 1: no <script>, <style>, on*= , style=, javascript: URLs or meta refresh in
 * any HTML, XHTML, SVG or XML file. Attributes may follow "/", a quote or space.
 */
export function gateNoInlineCode(dist) {
	const errs = [];
	for (const f of walk(dist)) {
		if (!/\.(html?|xhtml|svg|xml)$/i.test(f)) continue;
		const name = rel(dist, f);
		const src = read(f);
		// Whole-file check: a stray quote can make the tag scanner swallow a real <script>.
		const el = /<\s*\/?\s*(script|style)\b/i.exec(src);
		if (el) errs.push(`${name}: <${el[1].toLowerCase()}> element`);
		for (const m of src.matchAll(TAG)) {
			const tag = m[1].toLowerCase();
			const attrs = m[2];
			if (/(?:^|[\s"'/])on[a-z]+\s*=/i.test(attrs)) errs.push(`${name}: on*= attribute on <${tag}>`);
			if (/(?:^|[\s"'/])style\s*=/i.test(attrs)) errs.push(`${name}: style= attribute on <${tag}>`);
			for (const a of ['href', 'src', 'action', 'formaction', 'data', 'xlink:href']) {
				const v = attrOf(attrs, a);
				if (v !== null && /^[\s\x00-\x20]*javascript:/i.test(v)) errs.push(`${name}: javascript: URL in ${a} on <${tag}>`);
			}
			if (tag === 'meta' && /^\s*refresh\s*$/i.test(attrOf(attrs, 'http-equiv') ?? '')) errs.push(`${name}: <meta http-equiv="refresh">`);
		}
	}
	return errs;
}

/**
 * Parse a Cloudflare _headers file. Returns [{ pattern, headers: [[name, value]] }].
 * A "! Name" detach line is kept as ['!', name]. "#" lines are comments. Rules whose
 * pattern is a host (https://...) are kept only with { hosts: true }.
 */
export function parseHeaders(text, { hosts = false } = {}) {
	const rules = [];
	let cur = null;
	for (const line of text.split('\n')) {
		if (line.trim() === '' || line.trim().startsWith('#')) continue;
		if (!/^\s/.test(line)) {
			const pat = line.trim();
			cur = pat.startsWith('/') || (hosts && /^https?:\/\//.test(pat)) ? { pattern: pat, headers: [] } : null;
			if (cur) rules.push(cur);
		} else if (cur) {
			const t = line.trim();
			if (t.startsWith('!')) cur.headers.push(['!', t.slice(1).trim()]);
			else {
				const i = t.indexOf(':');
				if (i > 0) cur.headers.push([t.slice(0, i).trim(), t.slice(i + 1).trim()]);
			}
		}
	}
	return rules;
}

const directive = (csp, name) => {
	const parts = csp.split(';').map((d) => d.trim().split(/\s+/)).filter((d) => d[0] !== '');
	return parts.find((d) => d[0].toLowerCase() === name)?.slice(1) ?? null;
};
const isNone = (t) => t !== null && t.length === 1 && t[0].toLowerCase() === "'none'";

/** Gate 2: _headers has one /* rule with one CSP (script-src and default-src exactly 'none'). */
export function gateHeaders(dist) {
	const p = join(dist, '_headers');
	if (!existsSync(p)) return ['_headers is missing'];
	const rules = parseHeaders(read(p), { hosts: true });
	const errs = [];
	for (const r of rules) {
		for (const [k, v] of r.headers) {
			if (k === '!') errs.push(`_headers: "! ${v}" detaches a header under ${r.pattern}`);
			else if (/^content-security-policy-report-only$/i.test(k)) errs.push(`_headers: Content-Security-Policy-Report-Only under ${r.pattern}`);
		}
	}
	const csps = (r) => r.headers.filter(([k]) => /^content-security-policy$/i.test(k)).map(([, v]) => v);
	const roots = rules.filter((r) => r.pattern === '/*');
	if (roots.length !== 1) errs.push('_headers must have exactly one /* rule');
	else {
		const c = csps(roots[0]);
		if (c.length !== 1) errs.push('_headers /* rule must set exactly one Content-Security-Policy');
		else {
			if (!isNone(directive(c[0], 'script-src'))) errs.push("_headers CSP: script-src must be exactly 'none'");
			if (!isNone(directive(c[0], 'default-src'))) errs.push("_headers CSP: default-src must be exactly 'none'");
		}
	}
	for (const r of rules) {
		if (r.pattern === '/*') continue;
		for (const c of csps(r)) if (!isNone(directive(c, 'script-src'))) errs.push(`_headers: CSP under ${r.pattern} lacks script-src 'none'`);
	}
	return errs;
}

const RFC3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$/i;

/** Gate 3: security.txt has Contact and one Expires, 30 days to 11 months away. */
export function gateSecurityTxt(dist, now = Date.now()) {
	const p = join(dist, '.well-known', 'security.txt');
	if (!existsSync(p)) return ['.well-known/security.txt is missing'];
	const text = read(p);
	const errs = [];
	if (!/^Contact:[ \t]*\S/im.test(text)) errs.push('security.txt has no Contact field');
	const ex = [...text.matchAll(/^Expires:[ \t]*(.*?)[ \t]*$/gim)];
	if (ex.length === 0) return [...errs, 'security.txt has no Expires field'];
	if (ex.length > 1) return [...errs, 'security.txt has more than one Expires field'];
	if (!RFC3339.test(ex[0][1]) || Number.isNaN(Date.parse(ex[0][1]))) return [...errs, 'security.txt Expires is not an RFC 3339 date-time with a timezone'];
	const t = Date.parse(ex[0][1]);
	const max = new Date(now);
	max.setUTCMonth(max.getUTCMonth() + 11);
	if (t - now < MIN_EXPIRY_DAYS * DAY) errs.push(`security.txt Expires is less than ${MIN_EXPIRY_DAYS} days away`);
	if (t > max.getTime()) errs.push('security.txt Expires is more than 11 months away');
	return errs;
}

/** Gate 4: no Pages Functions or worker output. */
export function gateNoFunctions(dist) {
	return ['_worker.js', 'functions', '_routes.json'].filter((n) => existsSync(join(dist, n))).map((n) => `${n} must not exist`);
}

/** Gate 5: no /styleguide/ in a production build. */
export function gateNoStyleguide(dist) {
	return existsSync(join(dist, 'styleguide')) ? ['styleguide/ must not exist in a production build'] : [];
}

/** Gate 6: every HTML file under dist/docs is <slug>/index.html, and the slugs equal the allowlist. */
export function gateDocsRoutes(dist, expected) {
	const base = join(dist, 'docs');
	const errs = [];
	const got = [];
	for (const f of walk(base)) {
		if (!/\.html?$/i.test(f)) continue;
		const r = relative(base, f).split(sep).join('/');
		if (r === 'index.html') continue;
		if (!r.endsWith('/index.html')) errs.push(`docs/${r} is not a directory route`);
		else got.push(r.slice(0, -'/index.html'.length));
	}
	const want = new Set(expected);
	const have = new Set(got);
	return [
		...errs,
		...got.filter((s) => !want.has(s)).map((s) => `docs/${s}/ is published but not in the allowlist`),
		...expected.filter((s) => !have.has(s)).map((s) => `docs/${s}/ is in the allowlist but not built`),
	];
}

const ATTR = /\s(href|src)\s*=\s*(?:"([^"]*)"|'([^']*)')/gi;

/** Gate 7: every same-site href/src resolves to a file, and every #id exists. */
export function gateLinks(dist) {
	const errs = [];
	const root = resolve(dist);
	const pages = walk(dist).filter((f) => f.endsWith('.html'));
	const ids = new Map();
	const idsOf = (file) => {
		if (!ids.has(file)) {
			const set = new Set();
			for (const m of read(file).matchAll(TAG)) {
				const id = attrOf(m[2], 'id');
				if (id !== null) set.add(id);
				if (m[1].toLowerCase() === 'a') {
					const nm = attrOf(m[2], 'name');
					if (nm !== null) set.add(nm);
				}
			}
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
