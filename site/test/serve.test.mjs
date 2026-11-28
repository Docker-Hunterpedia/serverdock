import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createSiteServer, parseHeaders } from '../scripts/serve.mjs';
import { HEADERS, cleanup, goodDist, page } from './helpers.mjs';

test('parseHeaders keeps path rules and skips host rules', () => {
	const r = parseHeaders('/*\n  A: b\n  C: d: e\n/_astro/*\n  X: y\nhttps://:p.pages.dev/*\n  Z: z\n');
	assert.deepEqual(r.map((x) => x.pattern), ['/*', '/_astro/*']);
	assert.deepEqual(r[0].headers, [['A', 'b'], ['C', 'd: e']]);
});

test('serves pages with the CSP, a 404 page, and never escapes dist or serves _headers', async () => {
	const d = goodDist({ '404.html': page('<h1>nope</h1>'), '_astro/a.css': 'a{}', _headers: HEADERS + '/_astro/*\n  Cache-Control: immutable\n' });
	const srv = createSiteServer(d).listen(0, '127.0.0.1');
	await new Promise((r) => srv.once('listening', r));
	const base = `http://127.0.0.1:${srv.address().port}`;
	try {
		let r = await fetch(`${base}/about/`);
		assert.equal(r.status, 200);
		assert.match(r.headers.get('content-security-policy'), /script-src 'none'/);
		assert.equal(r.headers.get('cache-control'), null);
		r = await fetch(`${base}/_astro/a.css`);
		assert.match(r.headers.get('cache-control'), /immutable/);
		r = await fetch(`${base}/missing/`);
		assert.equal(r.status, 404);
		assert.match(await r.text(), /nope/);
		assert.equal((await fetch(`${base}/_headers`)).status, 404);
		assert.equal((await fetch(`${base}/%2e%2e/%2e%2e/etc/passwd`)).status, 404);
		assert.equal((await fetch(`${base}/%00`)).status, 404);
	} finally {
		srv.close();
		cleanup(d);
	}
});
