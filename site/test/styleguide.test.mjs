import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { gateNoStyleguide } from '../scripts/gates.mjs';

const root = fileURLToPath(new URL('..', import.meta.url));
function build(env) {
	const out = mkdtempSync(join(tmpdir(), 'site-build-'));
	const e = { ...process.env, ASTRO_TELEMETRY_DISABLED: '1' };
	delete e.SITE_STYLEGUIDE;
	const r = spawnSync('npx', ['--no-install', 'astro', 'build', '--outDir', out], { cwd: root, env: { ...e, ...env } });
	assert.equal(r.status, 0, r.stderr.toString() + r.stdout.toString());
	return out;
}

test('/styleguide/ is built only when SITE_STYLEGUIDE=1', () => {
	const prod = build({});
	const test_ = build({ SITE_STYLEGUIDE: '1' });
	try {
		assert.equal(existsSync(join(prod, 'styleguide')), false);
		assert.deepEqual(gateNoStyleguide(prod), []);
		assert.equal(existsSync(join(test_, 'styleguide', 'index.html')), true);
		assert.equal(gateNoStyleguide(test_).length, 1);
		const html = readFileSync(join(test_, 'styleguide', 'index.html'), 'utf8');
		for (const marker of ['class="skip-link"', 'class="table-region"', 'class="meter"', 'class="stages"', 'class="callout', 'class="chip"', 'aria-label="On this page"', 'aria-label="Docs"']) {
			assert.ok(html.includes(marker), marker);
		}
		// the sidebar comes before <main>, so the skip link jumps past it
		assert.ok(html.indexOf('aria-label="Docs"') < html.indexOf('<main'));
	} finally {
		rmSync(prod, { recursive: true, force: true });
		rmSync(test_, { recursive: true, force: true });
	}
});
