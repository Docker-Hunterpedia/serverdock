import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const tokensCss = readFileSync(new URL('../src/styles/tokens.css', import.meta.url), 'utf8');
const ogHtml = readFileSync(new URL('../../docs/assets/og.html', import.meta.url), 'utf8');

// Take the body of the first block that starts with `opener` (brace matching).
function block(css, opener, from = 0) {
	const at = css.indexOf(opener, from);
	assert.ok(at >= 0, `${opener} not found`);
	let depth = 0;
	for (let i = css.indexOf('{', at); i < css.length; i++) {
		if (css[i] === '{') depth++;
		if (css[i] === '}' && --depth === 0) return css.slice(css.indexOf('{', at) + 1, i);
	}
	throw new Error('unbalanced');
}
function vars(body) {
	const out = {};
	for (const m of body.matchAll(/(--[a-z0-9-]+):\s*([^;]+?);/g)) out[m[1]] = m[2].trim();
	return out;
}
const dark = vars(block(tokensCss, ':root {'));
const lightRaw = vars(block(block(tokensCss, '@media print, (prefers-color-scheme: light)'), ':root {'));
const light = { ...dark, ...lightRaw };

const resolve = (theme, name) => {
	let v = theme[name];
	while (v && v.startsWith('var(')) v = theme[v.slice(4, -1)];
	assert.match(v ?? '', /^#[0-9a-f]{6}$/i, `${name} must resolve to #rrggbb`);
	return v;
};
function lum(hex) {
	const c = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255).map((x) => (x <= 0.03928 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4));
	return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2];
}
export function ratio(a, b) {
	const [x, y] = [lum(a), lum(b)].sort((p, q) => q - p);
	return (x + 0.05) / (y + 0.05);
}

const SURFACES = ['--bg', '--bg2', '--card', '--code-bg'];
const TEXT = ['--text', '--muted', '--accent-text', '--accent2-text', '--warn-text', ...Object.keys(dark).filter((k) => k.startsWith('--prism-'))];

for (const [themeName, theme] of [['dark', dark], ['light', light]]) {
	test(`${themeName}: every text token is at least 4.5:1 on every surface`, () => {
		for (const t of TEXT) for (const s of SURFACES) {
			const r = ratio(resolve(theme, t), resolve(theme, s));
			assert.ok(r >= 4.5, `${t} on ${s}: ${r.toFixed(2)}`);
		}
	});
	test(`${themeName}: the stage dot and tick (--accent-text with --bg) are at least 4.5:1`, () => {
		for (const s of ['--bg', '--card']) assert.ok(ratio(resolve(theme, '--accent-text'), resolve(theme, s)) >= 4.5, s);
	});
	test(`${themeName}: --border and the focus ring are at least 3:1 on every surface`, () => {
		for (const t of ['--border', '--accent2-text']) for (const s of SURFACES) {
			const r = ratio(resolve(theme, t), resolve(theme, s));
			assert.ok(r >= 3, `${t} on ${s}: ${r.toFixed(2)}`);
		}
	});
	test(`${themeName}: the meter fill is at least 3:1 against its track, and the track outline against the surface`, () => {
		assert.ok(ratio(resolve(theme, '--meter-fill'), resolve(theme, '--meter-track')) >= 3);
		for (const s of SURFACES) assert.ok(ratio(resolve(theme, '--border'), resolve(theme, s)) >= 3, s);
	});
}

test('every Prism token used in components.css has a token in both themes', () => {
	const used = new Set([...readFileSync(new URL('../src/styles/components.css', import.meta.url), 'utf8').matchAll(/var\((--prism-[a-z-]+)\)/g)].map((m) => m[1]));
	assert.ok(used.size >= 20);
	for (const u of used) {
		assert.ok(dark[u], `${u} missing in dark`);
		assert.ok(lightRaw[u], `${u} missing in light`);
	}
	for (const k of Object.keys(dark).filter((k) => k.startsWith('--prism-'))) assert.ok(used.has(k), `${k} is defined but unused`);
});

test('the light theme sets every text token and surface that differs from dark', () => {
	for (const k of ['--bg', '--bg2', '--card', '--code-bg', '--text', '--muted', '--accent-text', '--accent2-text', '--warn-text', '--border', '--meter-track', '--meter-fill']) {
		assert.ok(lightRaw[k], `${k} missing in light`);
	}
});

test('the dark tokens match og.html :root', () => {
	const og = vars(block(ogHtml, ':root {'));
	const map = { '--bg': '--bg', '--bg2': '--bg2', '--line': '--line', '--text': '--text', '--muted': '--muted', '--accent': '--accent', '--accent2': '--accent2', '--warn': '--warn-text' };
	for (const [ogName, ours] of Object.entries(map)) {
		assert.equal(dark[ours].toLowerCase(), og[ogName].toLowerCase(), `${ours} differs from og.html ${ogName}`);
	}
});

test('ratio() matches known values', () => {
	assert.equal(ratio('#000000', '#ffffff').toFixed(1), '21.0');
	assert.equal(ratio('#777777', '#ffffff').toFixed(2), '4.48');
});
