// Gates 8 and 9 of plan 0014: page weight and dist total. Only node: built-ins.
//
//   node scripts/page-weight.mjs <dist> [budgets.json]
//
// One metric: each file compressed with gzip -9, summed (the dist total is raw
// bytes on disk). 1 KB = 1000 bytes, like the web/ size check. Requests are 1
// (the HTML) plus the unique same-site resources named in link/img/script
// attributes and in CSS url(). Attribute regexes are fine here: the input is
// our own build output, and the Playwright "no other host" check backs it up.
import { appendFileSync, existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { dirname, join, posix, relative, resolve, sep } from 'node:path';
import { constants, gzipSync } from 'node:zlib';
import { fileURLToPath } from 'node:url';

const KB = 1000;
export const LIMITS = {
	cssFiles: 1,
	cssBytes: 15 * KB,
	homeBytes: 80 * KB,
	homeRequests: 5, // first view, lazy screenshots not counted (Revision 3)
	pageBytes: 150 * KB,
	htmlBytes: 60 * KB,
	distBytes: 5 * 1000 * KB,
	distFiles: 500,
	// Screenshots (gate 11, Revision 3): raw bytes, worst case per <picture>
	homeImageBytes: 190 * KB,
	featuresImageBytes: 700 * KB,
	eagerBytes: 120 * KB,
	eagerCount: 1,
	screenTotal: 2000 * KB,
	screenFileCaps: { 800: 50 * KB, 1600: 120 * KB, 300: 25 * KB, 600: 60 * KB },
};

const gz = (buf) => gzipSync(buf, { level: constants.Z_BEST_COMPRESSION }).length;

function walk(dir) {
	return readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
		const p = join(dir, e.name);
		return e.isDirectory() ? walk(p) : [p];
	});
}

const TAG = /<([a-zA-Z][^\s/>]*)((?:"[^"]*"|'[^']*'|[^'">])*)>/g;
const attr = (s, name) => {
	const m = new RegExp(`\\s${name}\\s*=\\s*(?:"([^"]*)"|'([^']*)')`, 'i').exec(' ' + s);
	return m ? (m[1] ?? m[2]) : null;
};
const sameSite = (u) => u !== '' && !/^[a-zA-Z][a-zA-Z0-9+.-]*:/.test(u) && !u.startsWith('//');
const FONT = /\.(woff2?|ttf|otf|eot)$/i;
const JS = /\.(m?js|cjs)$/i;

/** The resource URLs (as written) a page names, with their kind. */
function pageRefs(html) {
	const refs = [];
	const add = (url, kind) => url && refs.push({ url, kind });
	for (const m of html.matchAll(TAG)) {
		const name = m[1].toLowerCase();
		const a = m[2];
		if (name === 'link') {
			const rels = (attr(a, 'rel') ?? '').toLowerCase().split(/\s+/);
			const href = attr(a, 'href');
			if (href === null) continue;
			if (rels.includes('stylesheet')) add(href, 'css');
			else if (rels.includes('icon')) add(href, 'icon');
			else if (rels.includes('manifest')) add(href, 'manifest');
			else if (rels.includes('modulepreload') || rels.includes('preload')) add(href, 'preload');
		} else if (name === 'script') {
			refs.push({ url: attr(a, 'src') ?? '', kind: 'script' });
		} else {
			if (['img', 'source', 'video', 'audio', 'embed', 'track', 'iframe', 'input'].includes(name)) add(attr(a, 'src'), 'img');
			if (name === 'video') add(attr(a, 'poster'), 'img');
			if (name === 'object') add(attr(a, 'data'), 'img');
			if (name === 'img' || name === 'source') {
				for (const cand of (attr(a, 'srcset') ?? '').split(',')) add(cand.trim().split(/\s+/)[0], 'img');
			}
		}
		const style = attr(a, 'style');
		if (style) for (const u of style.matchAll(/url\(\s*(?:"([^"]*)"|'([^']*)'|([^)\s]*))\s*\)/gi)) add(u[1] ?? u[2] ?? u[3], 'cssurl');
	}
	return refs;
}

const SCREENS = '/screens/';
const srcsetUrls = (v) => (v ?? '').split(',').map((c) => c.trim().split(/\s+/)[0]).filter(Boolean);

/**
 * The screenshot images of a page: one entry per <picture> (or lone <img>) that names /screens/ files.
 * `eager` is an image without loading="lazy"; its candidates are every file the browser could pick.
 */
export function screenPictures(html) {
	const out = [];
	const blocks = [...html.matchAll(/<picture\b[\s\S]*?<\/picture>/gi)].map((m) => m[0]);
	const rest = html.replace(/<picture\b[\s\S]*?<\/picture>/gi, '');
	for (const b of [...blocks, ...[...rest.matchAll(/<img\b[^>]*>/gi)].map((m) => m[0])]) {
		const urls = [];
		let eager = false;
		for (const m of b.matchAll(TAG)) {
			const name = m[1].toLowerCase();
			if (name === 'img') {
				eager = (attr(m[2], 'loading') ?? '').toLowerCase() !== 'lazy';
				urls.push(attr(m[2], 'src'), ...srcsetUrls(attr(m[2], 'srcset')));
			} else if (name === 'source') urls.push(attr(m[2], 'src'), ...srcsetUrls(attr(m[2], 'srcset')));
		}
		const cands = [...new Set(urls.filter((u) => u && u.startsWith(SCREENS)).map((u) => u.split('#')[0].split('?')[0]))];
		if (cands.length > 0) out.push({ eager, cands });
	}
	return out;
}

/** Validate budgets.json exceptions. Returns a list of problems (empty when valid). */
export function validateExceptions(list) {
	if (!Array.isArray(list)) return ['"exceptions" must be an array'];
	const errs = [];
	list.forEach((e, i) => {
		const at = `exceptions[${i}]`;
		if (typeof e !== 'object' || e === null) return errs.push(`${at} must be an object`);
		if (typeof e.page !== 'string' || !e.page.startsWith('/')) errs.push(`${at}.page must be a string starting with /`);
		if (typeof e.html_kb !== 'number' || !Number.isFinite(e.html_kb) || e.html_kb <= 0 || e.html_kb > 100) errs.push(`${at}.html_kb must be a number above 0 and at most 100`);
		if (typeof e.reason !== 'string' || e.reason.trim() === '') errs.push(`${at}.reason must be a non-empty string`);
	});
	return errs;
}

/**
 * Measure every HTML page and the dist totals.
 * @returns {{ rows: object[], errs: string[], dist: { files: number, bytes: number } }}
 */
export function measure(dist, exceptions = [], limits = LIMITS) {
	const root = resolve(dist);
	const errs = [];
	const all = walk(root);
	const bytes = all.reduce((s, f) => s + statSync(f).size, 0);
	if (bytes > limits.distBytes) errs.push(`dist total ${bytes} bytes is over ${limits.distBytes}`);
	if (all.length > limits.distFiles) errs.push(`dist has ${all.length} files, over ${limits.distFiles}`);

	// every screenshot file, referenced or not: per-file caps by width, and the total
	const shots = all.filter((f) => relative(root, f).split(sep).join('/').startsWith('screens/') && f.endsWith('.webp'));
	let shotTotal = 0;
	for (const f of shots) {
		const size = statSync(f).size;
		shotTotal += size;
		const w = Number(/-(\d+)\.webp$/.exec(f)?.[1]);
		const cap = limits.screenFileCaps[w];
		if (!cap) errs.push(`screens/${relative(join(root, 'screens'), f)}: width ${w} has no cap (800, 1600, 300 or 600)`);
		else if (size > cap) errs.push(`screens/${relative(join(root, 'screens'), f)}: ${size} bytes, over the ${cap} cap for ${w} w`);
	}
	if (shotTotal > limits.screenTotal) errs.push(`screenshot files total ${shotTotal} bytes, over ${limits.screenTotal}`);

	const rows = [];
	for (const file of all.filter((f) => f.endsWith('.html')).sort()) {
		const rf = relative(root, file).split(sep).join('/');
		const route = rf === 'index.html' ? '/' : '/' + rf.replace(/index\.html$/, '');
		const htmlBuf = readFileSync(file);
		const html = gz(htmlBuf);
		const urls = new Map(); // site path -> kind
		const bad = [];
		const addUrl = (u, base, kind) => {
			if (!sameSite(u) || u.startsWith(SCREENS)) return;
			const clean = u.split('#')[0].split('?')[0];
			const path = clean.startsWith('/') ? clean : posix.resolve(posix.dirname(base), clean);
			if (!urls.has(path)) urls.set(path, kind);
		};
		let scripts = 0;
		for (const r of pageRefs(htmlBuf.toString('utf8'))) {
			if (r.kind === 'script') scripts++;
			addUrl(r.url, '/' + rf, r.kind);
		}
		// url() inside linked CSS
		for (const [path, kind] of [...urls]) {
			if (kind !== 'css') continue;
			const f = join(root, path);
			if (!existsSync(f)) continue;
			for (const m of readFileSync(f, 'utf8').matchAll(/url\(\s*(?:"([^"]*)"|'([^']*)'|([^)\s]*))\s*\)/gi)) {
				addUrl(m[1] ?? m[2] ?? m[3], path, 'cssurl');
			}
		}
		// gate 11: screenshots, in raw bytes, worst case per <picture>
		const pics = screenPictures(htmlBuf.toString('utf8'));
		const sizeOf = (u) => {
			const f = join(root, u);
			return existsSync(f) && statSync(f).isFile() ? statSync(f).size : null;
		};
		let imageWorst = 0;
		let eagerWorst = 0;
		let eagerCount = 0;
		const imgFail = [];
		for (const p of pics) {
			const sizes = p.cands.map((u) => [u, sizeOf(u)]);
			for (const [u, n] of sizes) if (n === null) imgFail.push(`${u} is named but missing`);
			const worst = Math.max(0, ...sizes.map(([, n]) => n ?? 0));
			imageWorst += worst;
			if (p.eager) {
				eagerCount++;
				eagerWorst += worst;
			}
		}
		if (pics.length > 0 && route !== '/' && route !== '/features/') imgFail.push('screenshots are allowed on Home and Features only');
		if (eagerCount > limits.eagerCount) imgFail.push(`${eagerCount} eager images, at most ${limits.eagerCount}`);
		if (eagerWorst > limits.eagerBytes) imgFail.push(`the eager image is ${eagerWorst} bytes, over ${limits.eagerBytes}`);
		const imageCeiling = route === '/' ? limits.homeImageBytes : route === '/features/' ? limits.featuresImageBytes : 0;
		if (pics.length > 0 && imageWorst > imageCeiling) imgFail.push(`screenshots ${imageWorst} bytes (worst case), over ${imageCeiling}`);
		let total = html;
		let js = scripts > 0 ? 1 : 0; // an inline or external <script> is a JS violation by itself
		let fonts = 0;
		let cssFiles = 0;
		let cssBytes = 0;
		for (const [path, kind] of urls) {
			const f = join(root, path);
			if (!existsSync(f) || !statSync(f).isFile()) {
				bad.push(path);
				continue;
			}
			const size = gz(readFileSync(f));
			total += size;
			if (JS.test(path)) js += size || 1;
			if (FONT.test(path)) fonts += size || 1;
			if (kind === 'css') {
				cssFiles++;
				cssBytes += size;
			}
		}
		const requests = 1 + urls.size + eagerCount; // first view: lazy screenshots are not requested
		const ex = exceptions.find((e) => e.page === route);
		const htmlCeiling = ex ? ex.html_kb * KB : limits.htmlBytes;
		const pageCeiling = route === '/' ? limits.homeBytes : limits.pageBytes;
		const fail = [];
		if (js > 0) fail.push('JavaScript is not allowed');
		if (fonts > 0) fail.push('webfonts are not allowed');
		if (cssFiles > limits.cssFiles) fail.push(`${cssFiles} CSS files, at most ${limits.cssFiles}`);
		if (cssBytes > limits.cssBytes) fail.push(`CSS ${cssBytes} bytes over ${limits.cssBytes}`);
		if (html > htmlCeiling) fail.push(`HTML ${html} bytes over ${htmlCeiling}`);
		if (total > pageCeiling) fail.push(`page total ${total} bytes over ${pageCeiling}`);
		if (route === '/' && requests > limits.homeRequests) fail.push(`${requests} requests, at most ${limits.homeRequests}`);
		for (const b of bad) fail.push(`${b} is named but missing`);
		fail.push(...imgFail);
		for (const f of fail) errs.push(`${route}: ${f}`);
		rows.push({ route, html, css: cssBytes, total, requests, images: imageWorst, ok: fail.length === 0 });
	}
	return { rows, errs, dist: { files: all.length, bytes } };
}

export function table(res) {
	const kb = (n) => (n / KB).toFixed(1);
	const lines = ['| Page | HTML (gzip KB) | CSS (gzip KB) | Total (gzip KB) | Screenshots (raw KB, worst case) | First-view requests | Result |', '|---|---|---|---|---|---|---|'];
	for (const r of res.rows) lines.push(`| ${r.route} | ${kb(r.html)} | ${kb(r.css)} | ${kb(r.total)} | ${kb(r.images)} | ${r.requests} | ${r.ok ? 'ok' : 'FAIL'} |`);
	lines.push('', `dist: ${res.dist.files} files, ${kb(res.dist.bytes)} KB raw`);
	return lines.join('\n');
}

if (process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1])) {
	const [distArg, budgetsArg] = process.argv.slice(2);
	if (!distArg || !existsSync(distArg)) {
		console.error('usage: page-weight.mjs <dist> [budgets.json]');
		process.exit(2);
	}
	let exceptions = [];
	const bpath = budgetsArg ?? join(dirname(fileURLToPath(import.meta.url)), '..', 'budgets.json');
	try {
		exceptions = JSON.parse(readFileSync(bpath, 'utf8')).exceptions ?? [];
	} catch (e) {
		console.error(`page-weight: cannot read budgets (${bpath}): ${e.message}`);
		process.exit(2);
	}
	const bad = validateExceptions(exceptions);
	if (bad.length) {
		for (const b of bad) console.error(`page-weight: budgets.json: ${b}`);
		process.exit(2);
	}
	const res = measure(distArg, exceptions);
	const t = table(res);
	console.log(t);
	if (process.env.GITHUB_STEP_SUMMARY) appendFileSync(process.env.GITHUB_STEP_SUMMARY, `### Page weight\n\n${t}\n`);
	for (const e of res.errs) console.error(`FAIL ${e}`);
	process.exit(res.errs.length ? 1 : 0);
}
