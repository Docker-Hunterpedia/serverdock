// A tiny static server for tests: serves dist and applies the path rules of
// dist/_headers (host-pattern rules are skipped), so Playwright runs under the
// real CSP. node: built-ins only.
//
//   node scripts/serve.mjs <dist> [port]      (listens on 127.0.0.1 only)
import { createServer } from 'node:http';
import { existsSync, readFileSync, statSync } from 'node:fs';
import { extname, join, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

const TYPES = {
	'.html': 'text/html; charset=utf-8', '.css': 'text/css; charset=utf-8', '.svg': 'image/svg+xml',
	'.png': 'image/png', '.txt': 'text/plain; charset=utf-8', '.xml': 'application/xml',
};

/** Parse _headers into [{ pattern, headers: [[name, value]] }], skipping https:// host rules. */
export function parseHeaders(text) {
	const rules = [];
	let cur = null;
	for (const line of text.split('\n')) {
		if (line.trim() === '') continue;
		if (!/^\s/.test(line)) {
			cur = line.trim().startsWith('/') ? { pattern: line.trim(), headers: [] } : null;
			if (cur) rules.push(cur);
		} else if (cur) {
			const i = line.indexOf(':');
			if (i > 0) cur.headers.push([line.slice(0, i).trim(), line.slice(i + 1).trim()]);
		}
	}
	return rules;
}
const matches = (pattern, path) => (pattern.endsWith('*') ? path.startsWith(pattern.slice(0, -1)) : path === pattern);

export function createSiteServer(dist) {
	const root = resolve(dist);
	const rules = existsSync(join(root, '_headers')) ? parseHeaders(readFileSync(join(root, '_headers'), 'utf8')) : [];
	return createServer((req, res) => {
		let path;
		try {
			path = decodeURIComponent(new URL(req.url, 'http://x').pathname);
		} catch {
			path = '/\0';
		}
		let file = resolve(join(root, path));
		let status = 200;
		const inside = !path.includes('\0') && (file === root || file.startsWith(root + sep));
		if (inside && existsSync(file) && statSync(file).isDirectory()) file = join(file, 'index.html');
		if (!inside || !existsSync(file) || !statSync(file).isFile() || file.endsWith(`${sep}_headers`)) {
			status = 404;
			file = join(root, '404.html');
		}
		const headers = { 'Content-Type': TYPES[extname(file)] ?? 'application/octet-stream' };
		for (const r of rules) if (matches(r.pattern, path)) for (const [k, v] of r.headers) headers[k] = v;
		res.writeHead(status, headers);
		res.end(req.method === 'HEAD' ? undefined : readFileSync(file));
	});
}

if (process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1])) {
	const [dist, port = '4321'] = process.argv.slice(2);
	if (!dist || !existsSync(dist)) {
		console.error('usage: serve.mjs <dist> [port]');
		process.exit(2);
	}
	createSiteServer(dist).listen(Number(port), '127.0.0.1', () => console.log(`serving ${dist} on http://127.0.0.1:${port}/`));
}
