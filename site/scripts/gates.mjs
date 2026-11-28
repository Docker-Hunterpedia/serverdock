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
				if (v !== null && /^[\s\x00-\x20]*javascript:/i.test(decodeUrl(v))) errs.push(`${name}: javascript: URL in ${a} on <${tag}>`);
			}
			if (tag === 'base' && attrOf(attrs, 'href') !== null) errs.push(`${name}: <base href>`);
			if (tag === 'iframe' && attrOf(attrs, 'srcdoc') !== null) errs.push(`${name}: <iframe srcdoc>`);
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
// Decode numeric and the few named entities that can spell "javascript:", then drop the
// tab, CR and LF that URL parsers ignore.
const NAMED = { tab: '\t', newline: '\n', colon: ':', lpar: '(', rpar: ')' };
const decodeUrl = (v) =>
	v
		.replace(/&#x([0-9a-f]+);?/gi, (_, h) => String.fromCodePoint(Math.min(parseInt(h, 16), 0x10ffff)))
		.replace(/&#(\d+);?/g, (_, d) => String.fromCodePoint(Math.min(Number(d), 0x10ffff)))
		.replace(/&([a-z]+);/gi, (m, n) => NAMED[n.toLowerCase()] ?? m)
		.replace(/[\t\r\n]/g, '');
const isNone = (t) => t !== null && t.length === 1 && t[0].toLowerCase() === "'none'";

// script-src-elem and script-src-attr override script-src (CSP Level 3): absent or exactly 'none'.
const overrides = (csp, where) =>
	['script-src-elem', 'script-src-attr']
		.filter((n) => directive(csp, n) !== null && !isNone(directive(csp, n)))
		.map((n) => `${where}: ${n} must be absent or exactly 'none'`);

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
			errs.push(...overrides(c[0], '_headers CSP'));
			if (!c[0].split(';').some((d) => d.trim().toLowerCase() === 'upgrade-insecure-requests')) errs.push('_headers CSP: upgrade-insecure-requests is missing');
		}
	}
	for (const r of rules) {
		if (r.pattern === '/*') continue;
		for (const c of csps(r)) {
			if (!isNone(directive(c, 'script-src'))) errs.push(`_headers: CSP under ${r.pattern} lacks script-src 'none'`);
			errs.push(...overrides(c, `_headers CSP under ${r.pattern}`));
		}
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

// ---- Gate 10: screenshots (plan 0014, Revision 3) -------------------------------------------------

/** The code-owned allowlist of captured route ids. Nothing under (auth)/ or settings/ may ever be added. */
export const SCREEN_ROUTES = [
	'(shell)/services/[project]/[name]/deploys/[number]',
	'(shell)/services',
	'(shell)/services/[project]/[name]/(service)',
	'(shell)/services/new',
];
/** The last sentence of every screenshot caption. */
export const DEMO_SENTENCE = 'Demo data: the states are staged with seeded rows and mocked API responses.';
const MANIFEST_KEYS = ['inputs_hash', 'captured_at', 'run_id', 'runner_image', 'chromium', 'growth', 'entries'];
const ENTRY_KEYS = ['id', 'route', 'viewport', 'theme', 'files'];
const FILE_KEYS = ['name', 'width', 'height', 'bytes'];
const WEBP_CHUNKS = new Set(['VP8 ', 'VP8L', 'VP8X', 'ALPH']);
const exactKeys = (o, keys) => o !== null && typeof o === 'object' && !Array.isArray(o) && Object.keys(o).length === keys.length && keys.every((k) => k in o);

/** The chunk ids of a WebP file, or an error string. Only RIFF/WEBP files made of VP8/VP8L/VP8X/ALPH chunks pass. */
export function webpProblem(buf) {
	if (buf.length < 20 || buf.toString('latin1', 0, 4) !== 'RIFF' || buf.toString('latin1', 8, 12) !== 'WEBP') return 'not a RIFF/WEBP file';
	if (buf.readUInt32LE(4) + 8 !== buf.length) return 'the RIFF size does not match the file size';
	let at = 12;
	while (at < buf.length) {
		if (at + 8 > buf.length) return 'truncated chunk header';
		const id = buf.toString('latin1', at, at + 4);
		const size = buf.readUInt32LE(at + 4);
		if (!WEBP_CHUNKS.has(id)) return `chunk "${id.trim()}" is not allowed (only VP8, VP8L, VP8X, ALPH)`;
		at += 8 + size + (size % 2);
		if (at > buf.length + 1) return 'a chunk runs past the end of the file';
	}
	return null;
}

/** Alt text problems for one text; `others` are the alt texts of the other figures on the page, `caption` its caption. */
export function altProblems(alt, caption, others = []) {
	const errs = [];
	const t = (alt ?? '').trim();
	if (t === '' || /^(…|\.{2,})$/.test(t) || /^[\w./-]+\.(webp|png|jpe?g|gif|svg)$/i.test(t)) errs.push('alt text is empty, "...", or a file name');
	if (/\b(screenshot|image of|picture of)\b/i.test(t)) errs.push('alt text says "screenshot", "image of" or "picture of"');
	if (t.length > 250) errs.push(`alt text is ${t.length} characters, over 250`);
	if (others.includes(alt)) errs.push('alt text is the same as another figure on the page');
	if (caption !== undefined && t === caption.trim()) errs.push('alt text is the same as its caption');
	return errs;
}

function readJson(path, errs, what) {
	try {
		return JSON.parse(read(path));
	} catch (e) {
		errs.push(`${what}: ${e.message}`);
		return null;
	}
}

/**
 * Gate 10. `siteDir` is the site/ directory (public/screens, src/data). `dist` is checked too when given.
 * Without a committed set (no manifest and no image files) only the text data is checked.
 */
export function gateScreens(siteDir, dist) {
	const errs = [];
	const screensDir = join(siteDir, 'public', 'screens');
	const repoRoot = resolve(siteDir, '..');
	const present = existsSync(screensDir) ? readdirSync(screensDir) : [];
	const images = present.filter((n) => n !== 'manifest.json');
	const manifestPath = join(screensDir, 'manifest.json');
	let ids = null;

	if (images.length > 0 && !present.includes('manifest.json')) errs.push('public/screens has image files but no manifest.json');
	if (present.includes('manifest.json')) {
		const m = readJson(manifestPath, errs, 'manifest.json');
		if (m) {
			if (!exactKeys(m, MANIFEST_KEYS)) errs.push(`manifest.json: keys must be exactly ${MANIFEST_KEYS.join(', ')}`);
			else {
				if (!/^[0-9a-f]{40}$/.test(m.inputs_hash)) errs.push('manifest.json: inputs_hash must be 40 hex characters');
				if (!RFC3339.test(m.captured_at ?? '')) errs.push('manifest.json: captured_at must be an RFC 3339 date-time');
				if (!/^\d+$/.test(m.run_id)) errs.push('manifest.json: run_id must be digits');
				for (const k of ['runner_image', 'chromium']) if (!/^[A-Za-z0-9._ -]{1,64}$/.test(m[k] ?? '')) errs.push(`manifest.json: ${k} must match ^[A-Za-z0-9._ -]{1,64}$`);
				if (!Array.isArray(m.growth) || !m.growth.every((g) => exactKeys(g, ['captured_at', 'bytes_added']) && RFC3339.test(g.captured_at) && Number.isInteger(g.bytes_added) && g.bytes_added >= 0)) errs.push('manifest.json: growth must be a list of { captured_at, bytes_added }');
				if (!Array.isArray(m.entries)) errs.push('manifest.json: entries must be a list');
				else {
					const listed = new Set();
					const dims = new Map(); // id + width -> [theme heights]
					ids = new Set();
					for (const e of m.entries) {
						if (!exactKeys(e, ENTRY_KEYS)) {
							errs.push(`manifest.json: an entry has keys other than ${ENTRY_KEYS.join(', ')}`);
							continue;
						}
						if (!/^[a-z0-9-]+$/.test(e.id)) errs.push(`manifest.json: bad id "${e.id}"`);
						ids.add(e.id);
						if (typeof e.route !== 'string' || e.route.includes('..') || e.route.startsWith('(auth)/') || e.route.includes('settings/') || !SCREEN_ROUTES.includes(e.route)) errs.push(`manifest.json: route "${e.route}" is not in the allowlist`);
						else if (!existsSync(join(repoRoot, 'web', 'src', 'routes', e.route, '+page.svelte'))) errs.push(`manifest.json: web/src/routes/${e.route}/+page.svelte does not exist`);
						if (!['desktop', 'phone'].includes(e.viewport)) errs.push(`manifest.json: viewport of ${e.id} must be desktop or phone`);
						if (!['dark', 'light'].includes(e.theme)) errs.push(`manifest.json: theme of ${e.id} must be dark or light`);
						if (!Array.isArray(e.files) || e.files.length === 0) errs.push(`manifest.json: ${e.id} has no files`);
						for (const f of Array.isArray(e.files) ? e.files : []) {
							if (!exactKeys(f, FILE_KEYS)) {
								errs.push(`manifest.json: a file of ${e.id} has keys other than ${FILE_KEYS.join(', ')}`);
								continue;
							}
							if (!new RegExp(`^${e.id}-(dark|light)-\\d+\\.webp$`).test(f.name) || !f.name.includes(`-${e.theme}-`)) errs.push(`manifest.json: file name "${f.name}" does not match its entry`);
							listed.add(f.name);
							const file = join(screensDir, f.name);
							if (!existsSync(file)) errs.push(`manifest.json: ${f.name} is listed but missing`);
							else {
								const buf = readFileSync(file);
								if (buf.length !== f.bytes) errs.push(`${f.name}: ${buf.length} bytes on disk, manifest says ${f.bytes}`);
								const p = webpProblem(buf);
								if (p) errs.push(`${f.name}: ${p}`);
							}
							const key = `${e.id}/${f.width}`;
							dims.set(key, [...(dims.get(key) ?? []), `${f.width}x${f.height}`]);
						}
					}
					for (const n of images) if (!listed.has(n)) errs.push(`public/screens/${n} is not listed in the manifest`);
					for (const [key, d] of dims) if (new Set(d).size > 1) errs.push(`${key}: both themes must have the same width and height (${d.join(', ')})`);
					for (const [key, d] of dims) if (d.length !== 2) errs.push(`${key}: each width needs exactly one file per theme`);
				}
			}
		}
	}

	// the text data: alt text and the first sentence of each caption
	const dataPath = join(siteDir, 'src', 'data', 'screens.json');
	if (existsSync(dataPath)) {
		const data = readJson(dataPath, errs, 'screens.json');
		if (data) {
			const alts = Object.entries(data).map(([id, v]) => [id, v?.alt]);
			for (const [id, v] of Object.entries(data)) {
				if (!exactKeys(v, ['alt', 'shows'])) {
					errs.push(`screens.json: ${id} must have exactly alt and shows`);
					continue;
				}
				if (typeof v.shows !== 'string' || !v.shows.trim().endsWith('.') || v.shows.includes('Demo data:')) errs.push(`screens.json: ${id}.shows must be one sentence ending in a period, without the Demo data sentence`);
				const others = alts.filter(([i]) => i !== id).map(([, a]) => a);
				for (const e of altProblems(v.alt, v.shows, others)) errs.push(`screens.json: ${id}: ${e}`);
			}
			if (ids) {
				for (const id of ids) if (!(id in data)) errs.push(`screens.json has no entry for manifest id ${id}`);
				for (const id of Object.keys(data)) if (!ids.has(id)) errs.push(`screens.json has ${id}, which is not in the manifest`);
			}
		}
	}

	// the built pages
	if (dist) {
		for (const f of walk(dist).filter((p) => p.endsWith('.html'))) {
			const html = read(f);
			const name = rel(dist, f);
			const alts = [];
			for (const m of html.matchAll(TAG)) {
				if (m[1].toLowerCase() !== 'img') continue;
				for (const a of ['width', 'height', 'alt']) if (attrOf(m[2], a) === null) errs.push(`${name}: <img> has no ${a}`);
				if (attrOf(m[2], 'decoding') !== 'async') errs.push(`${name}: <img> must have decoding="async"`);
				if (/^\/screens\//.test(attrOf(m[2], 'src') ?? '')) alts.push(attrOf(m[2], 'alt') ?? '');
			}
			for (const fig of html.matchAll(/<figure\b[^>]*\bclass="[^"]*\bscreen\b[^"]*"[^>]*>[\s\S]*?<\/figure>/g)) {
				const cap = /<figcaption[^>]*>([\s\S]*?)<\/figcaption>/.exec(fig[0]);
				const text = cap ? cap[1].replace(/<[^>]+>/g, '').replace(/\s+/g, ' ').trim() : '';
				if (!text.endsWith(DEMO_SENTENCE)) errs.push(`${name}: a screenshot caption must end with "${DEMO_SENTENCE}"`);
				const img = /<img\b[^>]*>/.exec(fig[0]);
				const alt = img ? attrOf(img[0], 'alt') : null;
				if (alt !== null && alt.trim() === text) errs.push(`${name}: a screenshot's alt text equals its caption`);
			}
			if (new Set(alts).size !== alts.length) errs.push(`${name}: two screenshots have the same alt text`);
		}
	}
	return errs;
}

// ---- Gate 10b: the Home stage list against the product's labels ----------------------------------------

/** The blue-green stage order, as the product runs it (web/e2e/seed.ts stageRows, internal/deploy). */
export const BLUE_GREEN_ORDER = ['pull', 'check', 'start', 'health', 'switch', 'commit', 'drain', 'stop_old'];

/** site/src/data/stages.json must use the exact on-screen labels of STAGE_LABELS (web/src/lib/services.ts, read as text). */
export function gateStages(siteDir) {
	const errs = [];
	const path = join(siteDir, 'src', 'data', 'stages.json');
	if (!existsSync(path)) return ['src/data/stages.json is missing'];
	const stages = readJson(path, errs, 'stages.json');
	if (!stages) return errs;
	const ts = join(siteDir, '..', 'web', 'src', 'lib', 'services.ts');
	if (!existsSync(ts)) return ['web/src/lib/services.ts is missing'];
	const body = /const STAGE_LABELS[^{]*\{([\s\S]*?)\n\};/.exec(read(ts));
	if (!body) return ['STAGE_LABELS was not found in web/src/lib/services.ts'];
	const labels = new Map([...body[1].matchAll(/(\w+):\s*'((?:[^'\\]|\\.)*)'/g)].map((m) => [m[1], m[2].replace(/\\'/g, "'")]));
	if (!Array.isArray(stages) || !stages.every((s) => exactKeys(s, ['key', 'label']))) return ['stages.json must be a list of { key, label }'];
	if (stages.map((s) => s.key).join() !== BLUE_GREEN_ORDER.join()) errs.push(`stages.json: the order must be ${BLUE_GREEN_ORDER.join(', ')}`);
	for (const s of stages) {
		if (!labels.has(s.key)) errs.push(`stages.json: "${s.key}" is not a key of STAGE_LABELS`);
		else if (labels.get(s.key) !== s.label) errs.push(`stages.json: label of ${s.key} is "${s.label}", STAGE_LABELS says "${labels.get(s.key)}"`);
	}
	return errs;
}

/** Run gates; returns { name, errs }[] . `expectedDocs` is the allowlist of doc slugs. */
export function runGates(dist, { distOnly = false, now = Date.now(), expectedDocs = [], siteDir } = {}) {
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
		if (siteDir) {
			out.push(['10 screens', gateScreens(siteDir, dist)]);
			out.push(['10 stages', gateStages(siteDir)]);
		}
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
	for (const g of runGates(dist, { distOnly, siteDir: fileURLToPath(new URL('..', import.meta.url)) })) {
		if (g.errs.length === 0) console.log(`ok   gate ${g.name}`);
		else {
			failed++;
			console.log(`FAIL gate ${g.name}`);
			for (const e of g.errs) console.log(`       ${e}`);
		}
	}
	process.exit(failed ? 1 : 0);
}
