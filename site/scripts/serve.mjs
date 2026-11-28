// A tiny static server for tests: serves dist and applies the path rules of
// dist/_headers (host-pattern rules are skipped), so Playwright runs under the
// real CSP. node: built-ins only.
//
//   node scripts/serve.mjs <dist> [port]      (listens on 127.0.0.1 only)
import { createServer } from 'node:http';
import { existsSync, readFileSync, statSync } from 'node:fs';
import { basename, extname, join, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseHeaders } from './gates.mjs';

export { parseHeaders };

const TYPES = {
	'.html': 'text/html; charset=utf-8', '.css': 'text/css; charset=utf-8', '.svg': 'image/svg+xml',
	'.png': 'image/png', '.txt': 'text/plain; charset=utf-8', '.xml': 'application/xml',
};

// WebKit upgrades http://127.0.0.1 to https because of upgrade-insecure-requests (Chromium exempts
// loopback), and this server is plain http. SERVE_NO_UPGRADE=1 drops that one directive from the
// CSP it sends; every other directive stays as in _headers. Tests only: never set in production.
const dropUpgrade = (v) => v.split(';').map((d) => d.trim()).filter((d) => d !== '' && d.toLowerCase() !== 'upgrade-insecure-requests').join('; ');

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
		if (!inside || !existsSync(file) || !statSync(file).isFile() || basename(file).toLowerCase() === '_headers') {
			status = 404;
			file = join(root, '404.html');
		}
		const headers = { 'Content-Type': TYPES[extname(file)] ?? 'application/octet-stream' };
		for (const r of rules) {
			if (!matches(r.pattern, path)) continue;
			for (const [k, v] of r.headers) {
				if (k === '!') {
					for (const h of Object.keys(headers)) if (h.toLowerCase() === v.toLowerCase()) delete headers[h];
				} else headers[k] = process.env.SERVE_NO_UPGRADE === '1' && /^content-security-policy$/i.test(k) ? dropUpgrade(v) : v;
			}
		}
		const body = existsSync(file) ? readFileSync(file) : 'Not found\n';
		if (!existsSync(file)) headers['Content-Type'] = 'text/plain; charset=utf-8';
		res.writeHead(status, headers);
		res.end(req.method === 'HEAD' ? undefined : body);
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
