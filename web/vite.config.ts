import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vite';
import { readFileSync } from 'node:fs';

// SvelteKit 3 reads its configuration from here (there is no svelte.config.js).
//
// A single-page app: every route falls back to index.html, which the Go server
// embeds (ADR 0005). Pre-compression is done by scripts/compress.mjs (gzip -9 next
// to each .js and .css) so the server can serve the files as they are. The server
// sets the CSP from the hashes of index.html's inline scripts (internal/api/spa.go):
// no CSP meta tag and no 'unsafe-inline'.
const config = {
	adapter: adapter({ pages: 'build', assets: 'build', fallback: 'index.html', precompress: false, strict: true })
};

// `make dev` serves Vite over HTTPS with a throwaway self-signed certificate (paths
// in VITE_DEV_KEY and VITE_DEV_CERT) and proxies /api to the server's HTTPS port.
// changeOrigin stays false: the Host header the browser sent (localhost:5173)
// reaches the server, and the browser's Origin (https://localhost:5173) equals
// https:// + Host, so the server's Origin rule is never loosened for development.
const key = process.env.VITE_DEV_KEY;
const cert = process.env.VITE_DEV_CERT;
const target = process.env.VITE_DEV_API ?? 'https://localhost:8443';

export default defineConfig({
	plugins: [sveltekit(config)],
	server: {
		https: key && cert ? { key: readFileSync(key), cert: readFileSync(cert) } : undefined,
		proxy: { '/api': { target, secure: false, changeOrigin: false } }
	}
});
