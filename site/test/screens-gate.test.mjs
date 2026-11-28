import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { BLUE_GREEN_ORDER, DEMO_SENTENCE, SCREEN_ROUTES, altProblems, gateScreenFiles, gateScreens, gateStages, webpProblem, webpSize } from '../scripts/gates.mjs';

const ROUTE = SCREEN_ROUTES[1]; // (shell)/services
const WEBP = Buffer.from('UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==', 'base64'); // a valid 1x1 lossless WebP, 34 bytes
const chunk = (id, n) => Buffer.concat([Buffer.from(id, 'latin1'), Buffer.from(new Uint32Array([n]).buffer), Buffer.alloc(n + (n % 2))]);
const riff = (...chunks) => {
	const body = Buffer.concat([Buffer.from('WEBP', 'latin1'), ...chunks]);
	return Buffer.concat([Buffer.from('RIFF', 'latin1'), Buffer.from(new Uint32Array([body.length]).buffer), body]);
};
/** A tiny WebP whose VP8L header declares w x h (the gate reads the header, not the pixels). */
const webpOf = (w, h) => {
	const data = Buffer.alloc(6);
	data[0] = 0x2f;
	data.writeUInt32LE((w - 1) | ((h - 1) << 14), 1);
	return riff(Buffer.concat([Buffer.from('VP8L', 'latin1'), Buffer.from(new Uint32Array([5]).buffer), data]));
};
const LEN = webpOf(1, 1).length;
const HASH = 'a'.repeat(40);

/** A repo root with site/ and web/ pieces. `over` replaces or (null) removes a manifest/data/file piece. */
function build(over = {}) {
	const root = mkdtempSync(join(tmpdir(), 'screens-gate-'));
	const site = join(root, 'site');
	const put = (path, content) => {
		mkdirSync(dirname(join(root, path)), { recursive: true });
		writeFileSync(join(root, path), content);
	};
	const file = (id, theme, w, h) => ({ name: `${id}-${theme}-${w}.webp`, width: w, height: h, bytes: LEN });
	const entry = (theme) => ({ id: 'services', route: ROUTE, viewport: 'desktop', theme, files: [file('services', theme, 800, 500), file('services', theme, 1600, 1000)] });
	const manifest = {
		inputs_hash: HASH, captured_at: '2026-11-28T10:00:00Z', run_id: '123', runner_image: 'ubuntu24-20260101.1', chromium: 'Chromium 150',
		growth: [{ captured_at: '2026-11-28T10:00:00Z', bytes_added: 136 }],
		entries: [entry('dark'), entry('light')],
		...over.manifest,
	};
	if (over.manifest !== null) for (const e of manifest.entries ?? []) for (const f of e.files ?? []) if (f?.name) put(`site/public/screens/${f.name}`, webpOf(f.width, f.height));
	if (over.manifest !== null) put('site/public/screens/manifest.json', JSON.stringify(manifest));
	put(`web/src/routes/${ROUTE}/+page.svelte`, '<h1>x</h1>');
	put('site/src/data/screens.json', JSON.stringify(over.data ?? { services: { alt: 'The services list with three services.', shows: 'The services list for project demo.' } }));
	for (const [p, c] of Object.entries(over.files ?? {})) put(p, c);
	return { root, site };
}
const check = (over, dist) => {
	const { root, site } = build(over);
	try { return gateScreens(site, dist); } finally { rmSync(root, { recursive: true, force: true }); }
};
const fails = (over, re) => {
	const errs = check(over);
	assert.ok(errs.length > 0, `expected a failure matching ${re}`);
	assert.match(errs.join('\n'), re);
};

test('gate 10: a good set passes, and so does a site with no set yet', () => {
	assert.deepEqual(check({}), []);
	const { root, site } = build({ manifest: null });
	try { assert.deepEqual(gateScreens(site), []); } finally { rmSync(root, { recursive: true, force: true }); }
});

test('gate 10: an image without a manifest fails', () => {
	const { root, site } = build({ manifest: null, files: { 'site/public/screens/x-dark-800.webp': WEBP } });
	try { assert.match(gateScreens(site).join(), /no manifest\.json/); } finally { rmSync(root, { recursive: true, force: true }); }
});

test('gate 10: routes outside the allowlist, under (auth)/ or settings/, with .., or without a route file', () => {
	const bad = (route) => ({ manifest: { entries: ['dark', 'light'].map((theme) => ({ id: 'services', route, viewport: 'desktop', theme, files: [{ name: `services-${theme}-800.webp`, width: 800, height: 500, bytes: LEN }, { name: `services-${theme}-1600.webp`, width: 1600, height: 1000, bytes: LEN }] })) } });
	for (const r of ['(auth)/setup', '(shell)/settings/security', '(shell)/services/../../x', '(shell)/other', '(auth)/login/2fa']) fails(bad(r), /not in the allowlist/);
	// an allowlisted route whose page file is missing
	const { root, site } = build({});
	try {
		rmSync(join(root, 'web', 'src', 'routes', ROUTE), { recursive: true });
		assert.match(gateScreens(site).join(), /does not exist/);
	} finally { rmSync(root, { recursive: true, force: true }); }
});

test('gate 10: the manifest schema is strict (unknown keys, bad fields)', () => {
	fails({ manifest: { extra: 1 } }, /keys must be exactly/);
	fails({ manifest: { inputs_hash: 'zz' } }, /inputs_hash/);
	fails({ manifest: { run_id: 'abc' } }, /run_id/);
	fails({ manifest: { runner_image: 'bad/image!' } }, /runner_image/);
	fails({ manifest: { chromium: '' } }, /chromium/);
	fails({ manifest: { captured_at: 'yesterday' } }, /captured_at/);
	fails({ manifest: { growth: [{ captured_at: '2026-11-28T10:00:00Z', bytes_added: -1 }] } }, /growth/);
	fails({ manifest: { growth: [{ captured_at: '2026-11-28T10:00:00Z', bytes_added: 1, commit: 'abc' }] } }, /growth/);
	const e = (patch) => ({ manifest: { entries: [{ id: 'services', route: ROUTE, viewport: 'desktop', theme: 'dark', files: [{ name: 'services-dark-800.webp', width: 800, height: 500, bytes: 34 }], ...patch }] } });
	fails(e({ commit: 'abc' }), /keys other than/);
	fails(e({ viewport: 'tablet' }), /viewport/);
	fails(e({ theme: 'sepia' }), /theme/);
	fails(e({ id: 'Bad Id' }), /bad id/);
	fails(e({ files: [{ name: 'services-dark-800.webp', width: 800, height: 500, bytes: 34, extra: 1 }] }), /keys other than/);
});

test('gate 10: files must be listed, exist, match their size and have the same dimensions in both themes', () => {
	fails({ files: { 'site/public/screens/stray-dark-800.webp': WEBP } }, /not listed in the manifest/);
	fails({ manifest: { entries: [{ id: 'services', route: ROUTE, viewport: 'desktop', theme: 'dark', files: [{ name: 'services-dark-800.webp', width: 800, height: 500, bytes: 99 }] }] } }, /bytes on disk/);
	const { root, site } = build({});
	try {
		rmSync(join(site, 'public', 'screens', 'services-dark-800.webp'));
		assert.match(gateScreens(site).join(), /listed but missing/);
	} finally { rmSync(root, { recursive: true, force: true }); }
	const light = (h) => ({ id: 'services', route: ROUTE, viewport: 'desktop', theme: 'light', files: [{ name: 'services-light-800.webp', width: 800, height: h, bytes: LEN }] });
	const dark = { id: 'services', route: ROUTE, viewport: 'desktop', theme: 'dark', files: [{ name: 'services-dark-800.webp', width: 800, height: 500, bytes: LEN }] };
	fails({ manifest: { entries: [dark, light(520)] } }, /same width and height/);
	fails({ manifest: { entries: [dark] } }, /exactly one file per theme/);
});

test('gate 10: the real image size from the header must match the manifest', () => {
	assert.deepEqual(webpSize(webpOf(800, 500)), [800, 500]);
	assert.deepEqual(webpSize(webpOf(1600, 1000)), [1600, 1000]);
	const e = (theme, w, h) => ({ id: 'services', route: ROUTE, viewport: 'desktop', theme, files: [{ name: `services-${theme}-800.webp`, width: w, height: h, bytes: LEN }] });
	// the files are written from the manifest, so overwrite one with a different real size
	const { root, site } = build({ manifest: { entries: [e('dark', 800, 500), e('light', 800, 500)] } });
	try {
		writeFileSync(join(site, 'public', 'screens', 'services-dark-800.webp'), webpOf(801, 500));
		assert.match(gateScreens(site).join(), /the file is 801x500, the manifest says 800x500/);
	} finally { rmSync(root, { recursive: true, force: true }); }
});

test('gate 10: WebP structure (magic, size, chunks; EXIF, XMP and ICCP fail)', () => {
	assert.equal(webpProblem(WEBP), null);
	assert.match(webpProblem(Buffer.from('not a webp file at all, really....')), /RIFF/);
	assert.match(webpProblem(riff(chunk('VP8L', 6), chunk('EXIF', 4))), /EXIF/);
	assert.match(webpProblem(riff(chunk('VP8X', 10), chunk('XMP ', 4), chunk('VP8L', 6))), /XMP/);
	assert.match(webpProblem(riff(chunk('VP8X', 10), chunk('ICCP', 4), chunk('VP8L', 6))), /ICCP/);
	assert.equal(webpProblem(riff(chunk('VP8X', 10), chunk('ALPH', 4), chunk('VP8 ', 6))), null);
	const trunc = riff(chunk('VP8L', 6));
	assert.match(webpProblem(trunc.subarray(0, trunc.length - 2)), /size/);
	// through the gate: a file with an EXIF chunk
	const bad = riff(chunk('VP8L', 6), chunk('EXIF', 4));
	fails({ manifest: { entries: ['dark', 'light'].map((theme) => ({ id: 'services', route: ROUTE, viewport: 'desktop', theme, files: [{ name: `services-${theme}-800.webp`, width: 800, height: 500, bytes: bad.length }] })) }, files: { 'site/public/screens/services-dark-800.webp': bad, 'site/public/screens/services-light-800.webp': bad } }, /EXIF/);
});

test('gate 10: alt text rules', () => {
	assert.deepEqual(altProblems('The services list.', 'Caption.', []), []);
	for (const [alt, re] of [['', /empty/], ['   ', /empty/], ['…', /empty/], ['...', /empty/], ['services-dark-800.webp', /file name/],
		['A screenshot of the list', /screenshot/], ['Image of the list', /image of/i], ['picture of the list', /picture of/i], ['x'.repeat(251), /over 250/]]) {
		assert.match(altProblems(alt, 'Caption.', []).join(), re, alt);
	}
	assert.match(altProblems('Same', 'Other.', ['Same']).join(), /another figure/);
	assert.match(altProblems('Same as caption.', 'Same as caption.', []).join(), /same as its caption/);
});

test('gate 10: screens.json duplicate alt, alt equal to caption, ids and the Demo data sentence', () => {
	const two = (a, b) => ({ data: { services: { alt: a, shows: 'The services list for project demo.' }, 'deploy-live': { alt: b, shows: 'The deploy page.' } } });
	const noManifest = (data) => build({ ...data, manifest: null });
	for (const [over, re] of [[two('Same alt.', 'Same alt.'), /another figure/], [{ data: { services: { alt: 'The list.', shows: 'The list.' } } }, /same as its caption/],
		[{ data: { services: { alt: 'A', shows: 'Has the Demo data: sentence.' } } }, /Demo data sentence/], [{ data: { services: { alt: 'A', shows: 'no period' } } }, /ending in a period/],
		[{ data: { services: { alt: 'A', shows: 'Ok.', extra: 1 } } }, /exactly alt and shows/]]) {
		const { root, site } = noManifest(over);
		try { assert.match(gateScreens(site).join(), re); } finally { rmSync(root, { recursive: true, force: true }); }
	}
	fails({ data: { services: { alt: 'The list.', shows: 'The list page.' }, extra: { alt: 'Another.', shows: 'More.' } } }, /not in the manifest/);
	fails({ data: {} }, /no entry for manifest id services/);
});

const page = (body) => `<!doctype html><html><body>${body}</body></html>`;
const fig = (cap, alt = 'The services list.') => `<figure class="screen"><picture><img src="/screens/services-dark-800.webp" width="800" height="500" alt="${alt}" decoding="async"></picture><figcaption>${cap}</figcaption></figure>`;
function distWith(html) {
	const d = mkdtempSync(join(tmpdir(), 'screens-dist-'));
	writeFileSync(join(d, 'index.html'), html);
	return d;
}
test('gate 10: built pages (img attributes, caption sentence, alt vs caption)', () => {
	const good = fig(`The list. Captured <time datetime="2026-11-28">28 Nov 2026</time>. ${DEMO_SENTENCE}`);
	const run = (html) => { const d = distWith(html); const { root, site } = build({}); try { return gateScreens(site, d); } finally { rmSync(d, { recursive: true, force: true }); rmSync(root, { recursive: true, force: true }); } };
	assert.deepEqual(run(page(good)), []);
	assert.match(run(page(fig('The list. Captured today.'))).join(), /must end with/);
	assert.match(run(page(fig(DEMO_SENTENCE, DEMO_SENTENCE))).join(), /equals its caption/);
	for (const attr of ['width="800" ', 'height="500" ', 'alt="The services list." ']) assert.match(run(page(good.replace(attr, ''))).join(), /<img> has no/, attr);
	assert.match(run(page(good.replace(' decoding="async"', ''))).join(), /decoding="async"/);
	assert.match(run(page(good + good)).join(), /same alt text/);
});

test('gate 10b: stages.json matches STAGE_LABELS in order and text', () => {
	const ts = (labels) => `export const x = 1;\nconst STAGE_LABELS: Record<string, string> = {\n${labels}\n};\n`;
	const LABELS = ["pull: 'Pull the image'", "check: 'Checks'", "start: 'Start the new version'", "health: 'Health check'", "switch: 'Switch traffic'", "commit: 'Record the switch'", "drain: 'Drain the old version'", "stop_old: 'Retire the old version'", "maintenance: 'Show the maintenance page'"].map((l) => `\t${l},`).join('\n');
	const label = { pull: 'Pull the image', check: 'Checks', start: 'Start the new version', health: 'Health check', switch: 'Switch traffic', commit: 'Record the switch', drain: 'Drain the old version', stop_old: 'Retire the old version' };
	const list = (order = BLUE_GREEN_ORDER, over = {}) => JSON.stringify(order.map((key) => ({ key, label: over[key] ?? label[key] })));
	const run = (stages, services = ts(LABELS)) => {
		const { root, site } = build({ files: { 'site/src/data/stages.json': stages, 'web/src/lib/services.ts': services } });
		try { return gateStages(site); } finally { rmSync(root, { recursive: true, force: true }); }
	};
	assert.deepEqual(run(list()), []);
	assert.match(run(list(BLUE_GREEN_ORDER, { health: 'Health check!' })).join(), /label of health/);
	assert.match(run(list([...BLUE_GREEN_ORDER].reverse())).join(), /order must be/);
	assert.match(run(list(BLUE_GREEN_ORDER.slice(1))).join(), /order must be/);
	assert.match(run(list(), ts("\tpull: 'Pull the image',")).join(), /not a key of STAGE_LABELS/);
	assert.match(run('{"not":"a list"}').join(), /list of/);
	assert.match(run(list(), 'export const nothing = 1;').join(), /was not found/);
});

test('dist screens files: valid WebP only, plus manifest.json (also in --dist-only)', () => {
	const d = mkdtempSync(join(tmpdir(), 'screens-dist-files-'));
	try {
		assert.deepEqual(gateScreenFiles(d), []);
		mkdirSync(join(d, 'screens'));
		writeFileSync(join(d, 'screens', 'a-dark-800.webp'), webpOf(800, 500));
		writeFileSync(join(d, 'screens', 'manifest.json'), '{}');
		assert.deepEqual(gateScreenFiles(d), []);
		writeFileSync(join(d, 'screens', 'b-dark-800.webp'), riff(chunk('VP8L', 6), chunk('EXIF', 4)));
		assert.match(gateScreenFiles(d).join(), /EXIF/);
		rmSync(join(d, 'screens', 'b-dark-800.webp'));
		writeFileSync(join(d, 'screens', 'notes.txt'), 'x');
		assert.match(gateScreenFiles(d).join(), /only \.webp files/);
	} finally { rmSync(d, { recursive: true, force: true }); }
});
