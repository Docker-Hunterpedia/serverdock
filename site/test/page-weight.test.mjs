import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { randomBytes } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { LIMITS, measure, validateExceptions } from '../scripts/page-weight.mjs';
import { cleanup, fixture, page } from './helpers.mjs';

const script = fileURLToPath(new URL('../scripts/page-weight.mjs', import.meta.url));
const css = '<link rel="stylesheet" href="/_astro/a.css">';
const noise = (n) => randomBytes(n).toString('base64'); // does not compress
const run = (files, exceptions = []) => {
	const d = fixture({ '_astro/a.css': 'a{color:red}', ...files });
	try { return measure(d, exceptions); } finally { cleanup(d); }
};

test('a small page passes and counts requests', () => {
	const r = run({ 'index.html': page('<img src="/i.png" alt="">', css + '<link rel="icon" href="/f.svg">'), 'i.png': 'x', 'f.svg': '<svg/>' });
	assert.deepEqual(r.errs, []);
	assert.equal(r.rows[0].requests, 4); // html + css + icon + img
});

test('the same resource twice is one request', () => {
	const r = run({ 'index.html': page('<img src="/i.png" alt=""><img src="/i.png" alt="">', css), 'i.png': 'x' });
	assert.equal(r.rows[0].requests, 3);
});

test('other hosts are not counted', () => {
	const r = run({ 'index.html': page('<img src="https://x.invalid/i.png" alt="">', css) });
	assert.equal(r.rows[0].requests, 2);
});

test('JavaScript is over budget at any size (external, inline, modulepreload)', () => {
	for (const head of ['<script src="/a.js"></script>', '<script>1</script>', '<link rel="modulepreload" href="/a.js">']) {
		const r = run({ 'index.html': page('', css + head), 'a.js': 'x' });
		assert.ok(r.errs.some((e) => /JavaScript/.test(e)), head);
	}
});

test('a webfont, also one named by CSS url(), is over budget', () => {
	const r = run({ 'index.html': page('', css), '_astro/a.css': '@font-face{src:url(/f.woff2)}', 'f.woff2': 'x' });
	assert.ok(r.errs.some((e) => /webfonts/.test(e)));
});

test('CSS url() resources are counted', () => {
	const r = run({ 'index.html': page('', css), '_astro/a.css': 'a{background:url("/bg.png")}', 'bg.png': 'x' });
	assert.equal(r.rows[0].requests, 3);
});

test('two CSS files, or one over 15 KB, fail', () => {
	let r = run({ 'index.html': page('', css + '<link rel="stylesheet" href="/b.css">'), 'b.css': 'a{}' });
	assert.ok(r.errs.some((e) => /CSS files/.test(e)));
	r = run({ 'index.html': page('', css), '_astro/a.css': noise(20000) });
	assert.ok(r.errs.some((e) => /CSS \d+ bytes over/.test(e)));
});

test('Home over 6 requests fails; another page over 6 requests does not', () => {
	const imgs = [1, 2, 3, 4, 5].map((n) => `<img src="/i${n}.png" alt="">`).join('');
	const files = Object.fromEntries([1, 2, 3, 4, 5].map((n) => [`i${n}.png`, 'x']));
	let r = run({ 'index.html': page(imgs, css), ...files });
	assert.ok(r.errs.some((e) => /7 requests/.test(e)));
	r = run({ 'about/index.html': page(imgs, css), ...files });
	assert.deepEqual(r.errs, []);
});

test('Home over 80 KB and another page over 150 KB fail', () => {
	let r = run({ 'index.html': page('<img src="/big.png" alt="">', css), 'big.png': noise(120000) });
	assert.ok(r.errs.some((e) => /^\/: page total/.test(e)));
	r = run({ 'about/index.html': page('<img src="/big.png" alt="">', css), 'big.png': noise(120000) });
	assert.deepEqual(r.errs, []); // 90 KB of base64 noise is under 150 KB
	r = run({ 'about/index.html': page('<img src="/big.png" alt="">', css), 'big.png': noise(230000) });
	assert.ok(r.errs.some((e) => /^\/about\/: page total/.test(e)));
});

test('HTML over 60 KB fails unless a named exception raises it', () => {
	const big = page(`<p>${noise(60000)}</p>`, css);
	assert.ok(run({ 'about/index.html': big }).errs.some((e) => /HTML \d+ bytes over 60000/.test(e)));
	assert.deepEqual(run({ 'about/index.html': big }, [{ page: '/about/', html_kb: 100, reason: 'test' }]).errs, []);
	// the exception is for that page only
	assert.ok(run({ 'other/index.html': big }, [{ page: '/about/', html_kb: 100, reason: 'test' }]).errs.length > 0);
});

test('dist total over 5 MB and over 500 files fail', () => {
	assert.ok(run({ 'index.html': page('', css), 'big.bin': Buffer.alloc(LIMITS.distBytes + 1) }).errs.some((e) => /dist total/.test(e)));
	const many = Object.fromEntries(Array.from({ length: 500 }, (_, i) => [`f/${i}.txt`, 'x']));
	assert.ok(run({ 'index.html': page('', css), ...many }).errs.some((e) => /files, over 500/.test(e)));
});

test('a named but missing resource fails', () => {
	assert.ok(run({ 'index.html': page('<img src="/gone.png" alt="">', css) }).errs.some((e) => /missing/.test(e)));
});

test('CLI: exit 0 under budget, 1 over, 2 on usage errors', () => {
	const d = fixture({ '_astro/a.css': 'a{}', 'index.html': page('', css) });
	const bad = fixture({ '_astro/a.css': 'a{}', 'index.html': page('<script>1</script>', css) });
	try {
		assert.equal(spawnSync('node', [script, d]).status, 0);
		assert.equal(spawnSync('node', [script, bad]).status, 1);
		assert.equal(spawnSync('node', [script]).status, 2);
	} finally {
		cleanup(d);
		cleanup(bad);
	}
});

test('srcset, source, video, object, embed, manifest and inline style url() are counted', () => {
	const r = run({
		'index.html': page(
			'<img src="/a.png" srcset="/b.png 1x, /c.png 2x" alt=""><video poster="/p.png"><source src="/v.mp4"></video><object data="/o.bin"></object><embed src="/e.bin"><p>x</p>',
			css + '<link rel="manifest" href="/m.json">',
		),
		...Object.fromEntries(['a.png', 'b.png', 'c.png', 'p.png', 'v.mp4', 'o.bin', 'e.bin', 'm.json'].map((f) => [f, 'x'])),
	});
	assert.equal(r.rows[0].requests, 10);
	const s = run({ 'index.html': page('<p style="background:url(/a.png)">x</p>', css), 'a.png': 'x' });
	assert.equal(s.rows[0].requests, 3);
});

test('validateExceptions rejects typos, strings, ranges and missing reasons', () => {
	const ok = { page: '/a/', html_kb: 100, reason: 'r' };
	assert.deepEqual(validateExceptions([ok]), []);
	for (const bad of [{ ...ok, html_kb: undefined, htmlKb: 100 }, { ...ok, html_kb: '100' }, { ...ok, html_kb: 101 }, { ...ok, html_kb: 0 },
		{ ...ok, html_kb: NaN }, { ...ok, page: 'a/' }, { ...ok, reason: ' ' }, null]) {
		assert.ok(validateExceptions([bad]).length >= 1, JSON.stringify(bad));
	}
	assert.ok(validateExceptions({}).length >= 1);
});

test('CLI: invalid, unparsable or missing budgets exit 2', () => {
	const d = fixture({ '_astro/a.css': 'a{}', 'index.html': page('', css) });
	const tmp = mkdtempSync(join(tmpdir(), 'budgets-'));
	const b = (name, text) => { const p = join(tmp, name); writeFileSync(p, text); return p; };
	try {
		for (const p of [
			b('typo.json', JSON.stringify({ exceptions: [{ page: '/a/', htmlKb: 100, reason: 'r' }] })),
			b('str.json', JSON.stringify({ exceptions: [{ page: '/a/', html_kb: '100', reason: 'r' }] })),
			b('bad.json', '{nope'),
			join(tmp, 'missing.json'),
		]) assert.equal(spawnSync('node', [script, d, p]).status, 2, p);
		assert.equal(spawnSync('node', [script, d, b('ok.json', '{"exceptions":[]}')]).status, 0);
	} finally { cleanup(d); cleanup(tmp); }
});

test('CLI writes the table to GITHUB_STEP_SUMMARY', () => {
	const d = fixture({ '_astro/a.css': 'a{}', 'index.html': page('', css) });
	const tmp = mkdtempSync(join(tmpdir(), 'summary-'));
	const sum = join(tmp, 'summary.md');
	try {
		const r = spawnSync('node', [script, d], { env: { ...process.env, GITHUB_STEP_SUMMARY: sum } });
		assert.equal(r.status, 0);
		assert.match(readFileSync(sum, 'utf8'), /### Page weight[\s\S]*\| \/ \|/);
	} finally { cleanup(d); cleanup(tmp); }
});

// ---- gate 11: screenshots (Revision 3) ----
const shot = (n, size) => [`screens/${n}.webp`, randomBytes(size)];
const pic = (id, { eager = false, dark = 800, light = 800 } = {}) =>
	`<picture><source media="print, (prefers-color-scheme: light)" srcset="/screens/${id}-light-${light}.webp ${light}w"><img src="/screens/${id}-dark-${dark}.webp" srcset="/screens/${id}-dark-${dark}.webp ${dark}w" width="800" height="500" alt="a" decoding="async"${eager ? ' fetchpriority="high"' : ' loading="lazy"'}></picture>`;
const files = (...pairs) => Object.fromEntries(pairs);

test('gate 11: a Home with one eager image and lazy images passes, and counts the worst case, not the sum', () => {
	const r = run({
		'index.html': page(pic('a', { eager: true }) + pic('b'), css),
		...files(shot('a-dark-800', 40000), shot('a-light-800', 40000), shot('b-dark-800', 40000), shot('b-light-800', 40000)),
	});
	assert.deepEqual(r.errs, []);
	assert.equal(r.rows[0].images, 80000); // 40 KB + 40 KB, one candidate per picture
	assert.equal(r.rows[0].requests, 3, 'html + css + the eager image; lazy images are not first-view requests');
});

test('gate 11: the heavier theme decides the worst case of a picture', () => {
	const r = run({ 'index.html': page(pic('a', { eager: true }), css), ...files(shot('a-dark-800', 10000), shot('a-light-800', 45000)) });
	assert.equal(r.rows[0].images, 45000);
});

test('gate 11: Home images over 190 KB (worst case) fail', () => {
	const r = run({
		'index.html': page(pic('a', { eager: true, dark: 1600, light: 1600 }) + pic('b', { dark: 1600, light: 1600 }), css),
		...files(shot('a-dark-1600', 119000), shot('a-light-1600', 100000), shot('b-dark-1600', 119000), shot('b-light-1600', 100000)),
	});
	assert.ok(r.errs.some((e) => /^\/: screenshots \d+ bytes \(worst case\), over 190000/.test(e)), r.errs.join());
});

test('gate 11: a second eager image fails, and the eager image is capped at 120 KB', () => {
	let r = run({ 'index.html': page(pic('a', { eager: true }) + pic('b', { eager: true }), css), ...files(shot('a-dark-800', 20000), shot('a-light-800', 20000), shot('b-dark-800', 20000), shot('b-light-800', 20000)) });
	assert.ok(r.errs.some((e) => /2 eager images, at most 1/.test(e)));
	r = run({ 'index.html': page(pic('a', { eager: true, dark: 1600, light: 1600 }), css), ...files(shot('a-dark-1600', 125000), shot('a-light-1600', 10000)) });
	assert.ok(r.errs.some((e) => /the eager image is 125000 bytes, over 120000/.test(e)));
});

test('gate 11: more than 5 first-view requests on Home fails', () => {
	const extra = '<img src="/i1.png" alt=""><img src="/i2.png" alt="">';
	const r = run({ 'index.html': page(extra + pic('a', { eager: true }), css + '<link rel="icon" href="/f.svg">'), 'i1.png': 'x', 'i2.png': 'y', 'f.svg': '<svg/>', ...files(shot('a-dark-800', 20000), shot('a-light-800', 20000)) });
	assert.ok(r.errs.some((e) => /6 requests, at most 5/.test(e)), r.errs.join());
});

test('gate 11: a screenshot file over its cap fails, by width', () => {
	for (const [name, size] of [['a-dark-800', 50001], ['a-dark-1600', 120001], ['a-dark-300', 25001], ['a-dark-600', 60001]]) {
		const r = run({ 'index.html': page('', css), ...files(shot(name, size)) });
		assert.ok(r.errs.some((e) => /over the \d+ cap/.test(e)), name);
	}
	assert.deepEqual(run({ 'index.html': page('', css), ...files(shot('a-dark-800', 50000), shot('a-dark-1600', 120000), shot('a-dark-300', 25000), shot('a-dark-600', 60000)) }).errs, []);
	assert.ok(run({ 'index.html': page('', css), ...files(shot('a-dark-999', 100)) }).errs.some((e) => /has no cap/.test(e)));
});

test('gate 11: the screenshot files in total are capped at 2 MB', () => {
	const many = Object.fromEntries(Array.from({ length: 18 }, (_, i) => shot(`s${i}-dark-1600`, 115000)));
	const r = run({ 'index.html': page('', css), ...many });
	assert.ok(r.errs.some((e) => /screenshot files total/.test(e)));
});

test('gate 11: screenshots on any page other than Home and Features fail; Features has a 700 KB ceiling', () => {
	const f = files(shot('a-dark-800', 20000), shot('a-light-800', 20000));
	assert.ok(run({ 'about/index.html': page(pic('a', { eager: true }), css), ...f }).errs.some((e) => /Home and Features only/.test(e)));
	assert.deepEqual(run({ 'features/index.html': page(pic('a', { eager: true }), css), ...f }).errs, []);
	const six = Object.fromEntries(Array.from({ length: 6 }, (_, i) => [`screens/p${i}-dark-1600.webp`, randomBytes(119000)]));
	const six2 = Object.fromEntries(Array.from({ length: 6 }, (_, i) => [`screens/p${i}-light-1600.webp`, randomBytes(119000)]));
	const html = Array.from({ length: 6 }, (_, i) => pic(`p${i}`, { eager: i === 0, dark: 1600, light: 1600 })).join('');
	const r = run({ 'features/index.html': page(html, css), ...six, ...six2 });
	assert.ok(r.errs.some((e) => /^\/features\/: screenshots \d+ bytes \(worst case\), over 700000/.test(e)), r.errs.join());
});

test('gate 11: a missing screenshot file is named', () => {
	assert.ok(run({ 'index.html': page(pic('a', { eager: true }), css) }).errs.some((e) => /is named but missing/.test(e)));
});

test('gate 11: a screenshot named in CSS url() fails', () => {
	const r = run({ 'index.html': page('', css), '_astro/a.css': 'a{background:url(/screens/a-dark-800.webp)}', ...files(shot('a-dark-800', 1000)) });
	assert.ok(r.errs.some((e) => /named in CSS url\(\)/.test(e)), r.errs.join());
});
