import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import {
	gateDocsRoutes, gateHeaders, gateLinks, gateNoFunctions, gateNoInlineCode,
	gateNoStyleguide, gateSecurityTxt, runGates,
} from '../scripts/gates.mjs';
import { NOW, cleanup, goodDist, page } from './helpers.mjs';

const script = fileURLToPath(new URL('../scripts/gates.mjs', import.meta.url));
const withDist = (overrides, fn) => {
	const d = goodDist(overrides);
	try { return fn(d); } finally { cleanup(d); }
};

test('a good dist passes every gate', () =>
	withDist({}, (d) => {
		for (const g of runGates(d, { now: NOW })) assert.deepEqual(g.errs, [], g.name);
	}));

test('gate 1: script, style element, on*= and style= are each rejected', () => {
	for (const [name, html] of [
		['script', '<script>1</script>'],
		['external script', '<script src="/a.js"></script>'],
		['style element', '<style>a{}</style>'],
		['onclick', '<a href="/" onclick="x()">a</a>'],
		['style attribute', '<p style="color:red">a</p>'],
		['svg style attribute', '<svg><rect style="fill:red"></rect></svg>'],
		['uppercase attr', '<p STYLE="a:b">a</p>'],
		['attribute after a quoted ">"', '<a title="a>b" onclick="x()">a</a>'],
	]) {
		withDist({ 'index.html': page(html) }, (d) => assert.notEqual(gateNoInlineCode(d).length, 0, name));
	}
});

test('gate 1: also checks .svg files, and ignores text that only looks like code', () => {
	withDist({ 'a.svg': '<svg><script>1</script></svg>' }, (d) => assert.equal(gateNoInlineCode(d).length, 1));
	withDist({ 'index.html': page('<p>use &lt;script&gt; and onclick= and style= in text</p>') }, (d) =>
		assert.deepEqual(gateNoInlineCode(d), []));
});

test('gate 2: _headers missing, or without script-src none', () => {
	withDist({ _headers: null }, (d) => assert.deepEqual(gateHeaders(d), ['_headers is missing']));
	withDist({ _headers: "/*\n  Content-Security-Policy: default-src 'self'\n" }, (d) => assert.equal(gateHeaders(d).length, 1));
});

test('gate 3: security.txt missing, no Expires, bad date, under 30 days', () => {
	withDist({ '.well-known/security.txt': null }, (d) => assert.equal(gateSecurityTxt(d, NOW).length, 1));
	withDist({ '.well-known/security.txt': 'Contact: x\n' }, (d) => assert.equal(gateSecurityTxt(d, NOW).length, 1));
	withDist({ '.well-known/security.txt': 'Expires: soon\n' }, (d) => assert.equal(gateSecurityTxt(d, NOW).length, 1));
	withDist({ '.well-known/security.txt': 'Expires: 2026-12-20T00:00:00Z\n' }, (d) => assert.equal(gateSecurityTxt(d, NOW).length, 1));
});

test('gate 3: 30 days is the boundary', () => {
	const ok = new Date(NOW + 31 * 86400000).toISOString();
	const bad = new Date(NOW + 29 * 86400000).toISOString();
	withDist({ '.well-known/security.txt': `Expires: ${ok}\n` }, (d) => assert.deepEqual(gateSecurityTxt(d, NOW), []));
	withDist({ '.well-known/security.txt': `Expires: ${bad}\n` }, (d) => assert.equal(gateSecurityTxt(d, NOW).length, 1));
});

test('gate 4: _worker.js, functions/ and _routes.json are each rejected', () => {
	for (const f of ['_worker.js', 'functions/a.js', '_routes.json']) {
		withDist({ [f]: 'x' }, (d) => assert.equal(gateNoFunctions(d).length, 1, f));
	}
	withDist({ '_worker.js/index.js': 'x' }, (d) => assert.equal(gateNoFunctions(d).length, 1));
});

test('gate 5: /styleguide/ is rejected in a production build', () => {
	withDist({ 'styleguide/index.html': page('x') }, (d) => assert.equal(gateNoStyleguide(d).length, 1));
});

test('gate 6: docs routes must equal the allowlist', () => {
	const files = { 'docs/index.html': page('i'), 'docs/vision/index.html': page('v') };
	withDist(files, (d) => {
		assert.deepEqual(gateDocsRoutes(d, ['vision']), []);
		assert.equal(gateDocsRoutes(d, []).length, 1); // published but not allowed
		assert.equal(gateDocsRoutes(d, ['vision', 'plans']).length, 1); // allowed but not built
	});
	withDist({}, (d) => assert.deepEqual(gateDocsRoutes(d, []), []));
});

test('gate 7: broken file, broken anchor, anchor on another page, and escapes', () => {
	withDist({ 'index.html': page('<a href="/nope/">x</a>') }, (d) => assert.equal(gateLinks(d).length, 1));
	withDist({ 'index.html': page('<a href="#missing">x</a>') }, (d) => assert.equal(gateLinks(d).length, 1));
	withDist({ 'index.html': page('<a href="/about/#missing">x</a>') }, (d) => assert.equal(gateLinks(d).length, 1));
	withDist({ 'index.html': page('<img src="/missing.png" alt="">') }, (d) => assert.equal(gateLinks(d).length, 1));
	withDist({ 'index.html': page('<a href="/../../etc/passwd">x</a>') }, (d) => assert.ok(gateLinks(d).length >= 1));
	withDist({ 'about/index.html': page('<a href="../">up</a><a href="../#top">anchor</a>'), 'index.html': page('<h1 id="top">x</h1>') }, (d) =>
		assert.deepEqual(gateLinks(d), []));
});

test('gate 7: other hosts, mailto and empty hrefs are not checked', () => {
	withDist({ 'index.html': page('<a href="https://x.invalid/">a</a><a href="//x.invalid/">b</a><a href="mailto:a@b.invalid">c</a><a href="">d</a>') }, (d) =>
		assert.deepEqual(gateLinks(d), []));
});

test('CLI: exit code 0 on a good dist, 1 on a bad one, 2 on usage errors', () => {
	const d = goodDist();
	const bad = goodDist({ '_worker.js': 'x' });
	try {
		assert.equal(spawnSync('node', [script, d]).status, 0);
		const r = spawnSync('node', [script, bad]);
		assert.equal(r.status, 1);
		assert.match(r.stdout.toString(), /FAIL gate 4/);
		assert.equal(spawnSync('node', [script]).status, 2);
		assert.equal(spawnSync('node', [script, '/nonexistent/dist']).status, 2);
	} finally {
		cleanup(d);
		cleanup(bad);
	}
});

test('CLI --dist-only: fails on worker output and on missing _headers/security.txt, skips gates 6 and 7', () => {
	for (const o of [{ '_worker.js': 'x' }, { 'functions/a.js': 'x' }, { '_routes.json': '{}' }, { _headers: null }, { '.well-known/security.txt': null }]) {
		const d = goodDist(o);
		try {
			assert.equal(spawnSync('node', [script, '--dist-only', d]).status, 1, JSON.stringify(Object.keys(o)));
		} finally { cleanup(d); }
	}
	// a broken link only fails the full run
	const d = goodDist({ 'index.html': page('<a href="/nope/">x</a>') });
	try {
		assert.equal(spawnSync('node', [script, '--dist-only', d]).status, 0);
		assert.equal(spawnSync('node', [script, d]).status, 1);
	} finally { cleanup(d); }
});
