import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';

export const NOW = Date.parse('2026-12-01T00:00:00Z');
export const HEADERS = "/*\n  Content-Security-Policy: default-src 'none'; script-src 'none'\n";
export const SECURITY = (exp) => `Contact: https://example.invalid/report\nExpires: ${exp}\n`;
export const page = (body, head = '') =>
	`<!doctype html><html lang="en"><head><title>t</title>${head}</head><body><main id="main">${body}</main></body></html>`;

/** Writes files ({ "rel/path": content }) into a fresh temp dir; returns the dir. */
export function fixture(files) {
	const dir = mkdtempSync(join(tmpdir(), 'site-fixture-'));
	for (const [p, c] of Object.entries(files)) {
		mkdirSync(dirname(join(dir, p)), { recursive: true });
		writeFileSync(join(dir, p), c);
	}
	return dir;
}
export const cleanup = (dir) => rmSync(dir, { recursive: true, force: true });

/** A dist that passes every gate. Pass overrides to change or (with null) remove files. */
export function goodDist(overrides = {}) {
	const files = {
		'index.html': page('<h1 id="top">Home</h1><a href="/about/">About</a><a href="/about/#sec">S</a><a href="#top">Top</a><a href="https://example.com/">x</a>'),
		'about/index.html': page('<h2 id="sec">S</h2><a href="/">Home</a>'),
		'_headers': HEADERS,
		'.well-known/security.txt': SECURITY('2027-10-15T00:00:00.000Z'),
		...overrides,
	};
	for (const k of Object.keys(files)) if (files[k] === null) delete files[k];
	return fixture(files);
}
