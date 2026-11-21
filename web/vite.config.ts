import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vite';
import { readFileSync } from 'node:fs';
import type { Plugin } from 'vite';

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

// SvelteKit's route announcer (an aria-live element in its root component) carries
// an inline style attribute, which our CSP (style-src 'self', no 'unsafe-inline')
// blocks: the announced title would show on screen. This plugin swaps the attribute
// for a class styled in src/app.css. It fails the build when the pinned SvelteKit's
// markup changes, so an upgrade cannot silently reintroduce the violation.
const ANNOUNCER_STYLE =
	'style="position: absolute; left: 0; top: 0; clip: rect(0 0 0 0); clip-path: inset(50%); overflow: hidden; white-space: nowrap; width: 1px; height: 1px"';
function announcerWithoutInlineStyle(): Plugin {
	return {
		name: 'serverdock-announcer-csp',
		enforce: 'pre',
		transform(code, id) {
			if (!id.replaceAll('\\', '/').endsWith('@sveltejs/kit/src/runtime/components/root.svelte')) return;
			if (!code.includes(ANNOUNCER_STYLE)) {
				throw new Error("SvelteKit's route announcer markup changed: update announcerWithoutInlineStyle() in vite.config.ts");
			}
			return { code: code.replace(ANNOUNCER_STYLE, 'class="sd-announcer"'), map: null };
		}
	};
}

export default defineConfig({
	plugins: [announcerWithoutInlineStyle(), sveltekit(config)],
	server: {
		https: key && cert ? { key: readFileSync(key), cert: readFileSync(cert) } : undefined,
		proxy: { '/api': { target, secure: false, changeOrigin: false } }
	}
});
