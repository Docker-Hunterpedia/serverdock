import type { Page } from '@playwright/test';
import { mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { hostname, networkInterfaces, tmpdir, userInfo } from 'node:os';
import { relative, resolve, sep } from 'node:path';
import type { components } from '../src/lib/api-types';
import { expect, signIn, test } from './fixtures';
import { mockAction, mockDeployGet, seedService } from './seed';
import { PASSWORD, startServer, type Admin, type TestServer } from './server';

// The screenshot capture for the public website (plan 0014, Revision 3). It opens the real ServerDock
// web UI on the real binary, with demo data, and captures six views in dark and light, as WebP files.
// Run only through playwright.screens.config.ts (`npm run screens`); `npm run e2e` never runs it.
//
// Honesty rules, in code:
//  - only the six routes in VIEWS can be captured (the allowlist; gate 10 has a second, code-owned one);
//  - mocks change data, never the UI: every answer below satisfies the generated API types;
//  - nothing from the real machine may appear in a capture (leak check, two layers);
//  - output goes to SCREENS_OUT, never into site/public/screens/ (only the CI artifact is committed).

type Schemas = components['schemas'];

/** The demo data. Everything on screen comes from here or from the UI itself. */
const DEMO = {
	project: 'demo',
	registry: 'registry.example.test/demo',
	// the only long hex strings a capture may show
	digests: ['sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef'],
	at: '2026-11-28T10:00:00.000Z'
};
const NOW = new Date(DEMO.at);
const iso = (minutesAgo: number) => new Date(NOW.getTime() - minutesAgo * 60_000).toISOString();

type Viewport = 'desktop' | 'phone';
/** `clipHeight`: CSS px of the main content that are captured. `scrollTo`: a heading to bring to the top first. */
type View = { id: string; route: string; viewport: Viewport; title: string; clipHeight: number; scrollTo?: string };

/** The route allowlist: the only views this spec can capture. (auth) routes and settings/ are never in it. */
export const VIEWS: View[] = [
	{ id: 'deploy-live', route: '(shell)/services/[project]/[name]/deploys/[number]', viewport: 'desktop', title: 'the live deploy page', clipHeight: 820 },
	{ id: 'services', route: '(shell)/services', viewport: 'desktop', title: 'the services list', clipHeight: 600 },
	{ id: 'service-detail', route: '(shell)/services/[project]/[name]/(service)', viewport: 'desktop', title: 'a service with its deploy history', clipHeight: 620, scrollTo: 'Deploys' },
	{ id: 'rollback-dialog', route: '(shell)/services/[project]/[name]/(service)', viewport: 'desktop', title: 'the roll back confirmation', clipHeight: 660 },
	{ id: 'new-service', route: '(shell)/services/new', viewport: 'desktop', title: 'deploy from an image', clipHeight: 560 },
	{ id: 'phone-deploy', route: '(shell)/services/[project]/[name]/deploys/[number]', viewport: 'phone', title: 'the live deploy page on a phone', clipHeight: 560, scrollTo: 'Stages' }
];

const THEMES = ['dark', 'light'] as const;
// Display widths: desktop figures show at up to 800 CSS px (files at 800 w and 1600 w); phone figures at 300 (300 w, 600 w).
const WIDTHS: Record<Viewport, [number, number]> = { desktop: [800, 1600], phone: [300, 600] };
// Per-file ceilings in bytes (plan section 4): desktop 1x 50 KB, 2x 120 KB; phone 1x 25 KB, 2x 60 KB.
const CAPS: Record<number, number> = { 800: 50_000, 1600: 120_000, 300: 25_000, 600: 60_000 };

// ---- typed demo answers (every one must satisfy the generated API types) -------------------

const service = (name: string, over: Partial<Schemas['ServicesGetOutput']> = {}): Schemas['ServicesGetOutput'] => ({
	service: `${DEMO.project}/${name}`,
	project: DEMO.project,
	name,
	image_ref: `${DEMO.registry}/${name}:1.4.0`,
	container_port: 8080,
	public_port: null,
	health: { kind: 'http', path: '/healthz', timeout_s: 60, passes: 3 },
	strategy: 'blue_green',
	drain_s: 10,
	stop_grace_s: 10,
	state: 'running',
	active_deploy: 6,
	last_deploy: { number: 6, kind: 'deploy', status: 'succeeded', created_at: iso(180), finished_at: iso(178), image_ref: `${DEMO.registry}/${name}:1.4.0` },
	routes_stale: false,
	routes_ready: true,
	warnings: [],
	created_at: iso(60 * 24 * 30),
	updated_at: iso(178),
	...over
});

const SERVICES = {
	services: [
		service('web', { image_ref: `${DEMO.registry}/web:2.1.0`, last_deploy: { number: 3, kind: 'deploy', status: 'succeeded', created_at: iso(1500), finished_at: iso(1498), image_ref: `${DEMO.registry}/web:2.1.0` }, active_deploy: 3 }),
		service('api', { active_deploy: 6, last_deploy: { number: 7, kind: 'deploy', status: 'running', created_at: iso(3), image_ref: `${DEMO.registry}/api:1.4.0` } }),
		service('worker', {
			image_ref: `${DEMO.registry}/worker:0.9.2`,
			container_port: null,
			health: { kind: 'running', timeout_s: 60, passes: 3 },
			needs_attention: 'previous_version_down',
			active_deploy: 2,
			last_deploy: { number: 2, kind: 'deploy', status: 'succeeded', created_at: iso(2900), finished_at: iso(2898), image_ref: `${DEMO.registry}/worker:0.9.2` }
		})
	]
} satisfies Schemas['ServicesListOutput'];

const deploy = (number: number, over: Partial<Schemas['DeploysListOutput']['deploys'][number]> = {}): Schemas['DeploysListOutput']['deploys'][number] => ({
	number,
	kind: 'deploy',
	status: 'succeeded',
	stage: 'done',
	image_ref: `${DEMO.registry}/api:1.${number}.0`,
	strategy: 'blue_green',
	active: false,
	was_live: false,
	log_kept: false,
	triggered_by: 'user',
	created_at: iso(60 * (30 - number * 4)),
	finished_at: iso(60 * (30 - number * 4) - 2),
	...over
});

/** A service whose history includes a rollback: #1 and #5 were live and can be rolled back to; #3 is a rollback to #1; #6 is live. */
const HISTORY = {
	service: `${DEMO.project}/api`,
	deploys: [
		deploy(6, { active: true, was_live: true, image_ref: `${DEMO.registry}/api:1.4.0`, log_kept: true }),
		deploy(5, { was_live: true, image_ref: `${DEMO.registry}/api:1.3.0` }),
		deploy(4, { status: 'failed', stage: 'health', reason_code: 'health_timeout', reason: 'the new version did not become healthy within 60 s', app_safe: 'yes', image_ref: `${DEMO.registry}/api:1.3.1`, log_kept: true }),
		deploy(3, { kind: 'rollback', was_live: true, image_ref: `${DEMO.registry}/api:1.1.0`, triggered_by: 'user' }),
		deploy(2, { was_live: true, image_ref: `${DEMO.registry}/api:1.2.0` }),
		deploy(1, { was_live: true, image_ref: `${DEMO.registry}/api:1.1.0` })
	]
} satisfies Schemas['DeploysListOutput'];

/** Deploy #7 of api: Pull the image, Checks and Start the new version done, Health check running. Typed in full: no untyped helper. */
const LIVE_DEPLOY = {
	service: `${DEMO.project}/api`,
	cancellable: true,
	deploy: {
		number: 7,
		kind: 'deploy',
		status: 'running',
		stage: 'health',
		image_ref: `${DEMO.registry}/api:1.4.0`,
		strategy: 'blue_green',
		active: false,
		was_live: false,
		log_kept: false,
		triggered_by: 'user',
		created_at: iso(3),
		started_at: iso(3)
	},
	stages: ['pull', 'check', 'start', 'health', 'switch', 'commit', 'drain', 'stop_old'].map((stage, i) => ({
		stage,
		state: i < 3 ? 'ok' : i === 3 ? 'running' : 'pending',
		...(i < 4 ? { started_at: iso(3) } : {}),
		...(i < 3 ? { finished_at: iso(2) } : {})
	}))
} satisfies Schemas['DeploysGetOutput'];

// ---- leak check ---------------------------------------------------------------------------

type Secrets = { exact: string[] };

function exactSecrets(token: string, admin: Admin, server: TestServer, extra: string[]): Secrets {
	const addresses: string[] = (Object.values(networkInterfaces()) as ({ internal: boolean; address: string }[] | undefined)[])
		.flatMap((list) => list ?? [])
		.filter((i) => !i.internal)
		.map((i) => i.address);
	const raw = [token, admin.secret, ...admin.recoveryCodes, PASSWORD, server.dataDir, tmpdir(), hostname(), userInfo().username, ...addresses, ...extra];
	// very short values would match anything; the long, specific ones are what matters
	return { exact: [...new Set(raw.filter((v) => typeof v === 'string' && v.length >= 4))] };
}

const PATTERNS: [string, RegExp][] = [
	['an API token (sd_...)', /\bsd_[A-Za-z0-9_-]{8,}/],
	['127.0.0.1', /127\.0\.0\.1/],
	['localhost', /localhost/i],
	['an email address', /[^\s@]+@[^\s@]+\.[A-Za-z]{2,}/],
	['a host name outside example.test', /\b(?:[a-z0-9-]+\.)+[a-z]{2,}\b/i]
];
const LONG_SECRETISH = /[A-Za-z0-9+/_=-]{32,}/g;

/** Returns what leaks in `text` (empty when clean). Pure, so it is testable on a planted value. */
export function findLeaks(text: string, secrets: Secrets, allowDigests: string[] = DEMO.digests): string[] {
	const hits: string[] = [];
	for (const s of secrets.exact) if (text.includes(s)) hits.push(`runtime value (${s.length} characters)`);
	let scrubbed = text;
	for (const d of allowDigests) scrubbed = scrubbed.split(d).join('');
	for (const [name, re] of PATTERNS) {
		for (const m of scrubbed.matchAll(new RegExp(re.source, re.flags.includes('g') ? re.flags : re.flags + 'g'))) {
			// a host name is fine when it is under example.test (or one of our own demo names without a dot)
			if (name.startsWith('a host name') && /\.test$/i.test(m[0])) continue;
			if (name.startsWith('a host name') && !/[a-z]{2,}$/i.test(m[0])) continue;
			hits.push(name);
			break;
		}
	}
	if (LONG_SECRETISH.test(scrubbed)) hits.push('a long secret-looking string');
	LONG_SECRETISH.lastIndex = 0;
	return hits;
}

const FORBIDDEN_ELEMENTS = 'canvas, [data-testid*="recovery" i], [data-testid*="totp" i], [data-testid*="qr" i], [aria-label*="QR" i], img[src^="data:"]';

/**
 * Scans what the capture will show: the text of every text node, and the value, placeholder and title
 * of every form control, whose box intersects the clip. (Text below the clip is not in the image.)
 */
async function assertNoLeaks(page: Page, secrets: Secrets, what: string, clip: { x: number; y: number; width: number; height: number }) {
	const texts = await page.evaluate((c) => {
		const hit = (r: DOMRect) => r.width > 0 && r.height > 0 && r.right > c.x && r.left < c.x + c.width && r.bottom > c.y && r.top < c.y + c.height;
		const out: string[] = [];
		const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
		for (let n = walker.nextNode(); n; n = walker.nextNode()) {
			const t = n.textContent ?? '';
			if (t.trim() === '') continue;
			const range = document.createRange();
			range.selectNodeContents(n);
			if (hit(range.getBoundingClientRect())) out.push(t);
		}
		for (const f of document.querySelectorAll<HTMLInputElement>('input, textarea, select')) {
			if (hit(f.getBoundingClientRect())) out.push(f.value ?? '', f.getAttribute('placeholder') ?? '', f.getAttribute('title') ?? '', f.getAttribute('aria-label') ?? '');
		}
		return out;
	}, clip);
	const found = findLeaks(texts.join('\n'), secrets);
	if (found.length && process.env.SCREENS_DEBUG) console.log(JSON.stringify(texts));
	expect(found, `the capture of ${what} would show: ${found.join(', ')}`).toEqual([]);
	expect(await page.locator(FORBIDDEN_ELEMENTS).count(), `${what} has a TOTP, QR or recovery-code element`).toBe(0);
}

// ---- encoding (no dependency: the libwebp encoder inside Chromium, through a canvas) --------

type Encoded = { width: number; height: number; bytes: number; quality: number; data: Buffer };

async function encodeAll(page: Page, png: Buffer, clip: { width: number; height: number }, widths: number[]): Promise<Encoded[]> {
	// a blank page of our own: nothing from the app can touch the canvas
	const blank = await page.context().newPage();
	try {
		await blank.goto('about:blank');
		const out: Encoded[] = [];
		for (const w of widths) {
			const h = Math.round((clip.height * w) / clip.width);
			let done: Encoded | undefined;
			for (let q = 0.8; q >= 0.4 && !done; q = Math.round((q - 0.05) * 100) / 100) {
				const b64 = await blank.evaluate(
					async ({ b64png, w, h, q }) => {
						const bin = Uint8Array.from(atob(b64png), (c) => c.charCodeAt(0));
						const bmp = await createImageBitmap(new Blob([bin], { type: 'image/png' }));
						const c = document.createElement('canvas');
						c.width = w;
						c.height = h;
						const ctx = c.getContext('2d')!;
						ctx.imageSmoothingEnabled = true;
						ctx.imageSmoothingQuality = 'high';
						ctx.drawImage(bmp, 0, 0, w, h);
						const blob: Blob = await new Promise((res, rej) => c.toBlob((x) => (x ? res(x) : rej(new Error('toBlob gave nothing'))), 'image/webp', q));
						const bytes = new Uint8Array(await blob.arrayBuffer());
						let s = '';
						for (let i = 0; i < bytes.length; i += 0x8000) s += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
						return btoa(s);
					},
					{ b64png: png.toString('base64'), w, h, q }
				);
				const data = Buffer.from(b64, 'base64');
				if (data.length <= CAPS[w]) done = { width: w, height: h, bytes: data.length, quality: q, data };
			}
			if (!done) throw new Error(`no WebP quality of at least 0.4 fits ${CAPS[w]} bytes at ${w} px: raise the cap with a written reason instead of making the text unreadable`);
			out.push(done);
		}
		return out;
	} finally {
		await blank.close();
	}
}

// ---- output -----------------------------------------------------------------------------------

function outDir(): string {
	const dir = process.env.SCREENS_OUT;
	if (!dir) throw new Error('SCREENS_OUT must name a scratch directory (never site/public/screens: only the CI artifact is committed)');
	const abs = resolve(dir);
	const rel = relative(resolve('..', 'site', 'public', 'screens'), abs);
	if (rel === '' || (!rel.startsWith('..') && !rel.startsWith(sep))) throw new Error('SCREENS_OUT must not be site/public/screens');
	return abs;
}

type Entry = {
	id: string;
	route: string;
	viewport: Viewport;
	theme: (typeof THEMES)[number];
	files: { name: string; width: number; height: number; bytes: number }[];
};
const entries: Entry[] = [];
let out = '';

test.beforeAll(() => {
	out = outDir();
	rmSync(out, { recursive: true, force: true });
	mkdirSync(out, { recursive: true });
});

test.afterAll(() => {
	writeFileSync(`${out}/entries.json`, JSON.stringify({ entries }, null, '\t') + '\n');
});

// ---- the six views ------------------------------------------------------------------------------

test.use({ storageState: { cookies: [], origins: [] }, reducedMotion: 'reduce', colorScheme: 'dark' });

/** Starts a server with the first run done, signs the page in, fixes the clock. Returns the leak-check secrets. */
async function open(page: Page, server: TestServer): Promise<Secrets> {
	const token = server.token(); // read before the first run consumes it
	const admin = await server.firstRun();
	await page.clock.install({ time: NOW });
	await page.clock.pauseAt(new Date(NOW.getTime() + 1));
	await signIn(page, server, admin);
	const session = await page.request.get(`${server.baseURL}/api/v1/auth/session`);
	const { csrf } = (await session.json()) as { csrf: string };
	const cookies = (await page.context().cookies()).map((c) => c.value);
	return exactSecrets(token, admin, server, [csrf, ...cookies]);
}

async function capture(page: Page, server: TestServer, secrets: Secrets, view: View, prepare?: () => Promise<void>) {
	expect(VIEWS.map((v) => v.id), 'the view is in the allowlist').toContain(view.id);
	const main = page.locator('main');
	await expect(main).toBeVisible();
	if (prepare) await prepare();
	await page.evaluate(() => window.scrollTo(0, 0));
	if (view.scrollTo) {
		await page.getByRole('heading', { name: view.scrollTo, exact: true }).first().evaluate((h) => h.scrollIntoView({ block: 'start' }));
		// leave room for the phone's top bar, which stays on screen while scrolling
		await page.evaluate(() => window.scrollBy(0, -(16 + (document.querySelector('header.bar')?.getBoundingClientRect().height ?? 0))));
	}
	await page.waitForTimeout(300);
	// the clip is inside <main>: the status footer (the capture server's real RAM, CPU and version) is never in it
	const box = (await main.boundingBox())!;
	const top = Math.max(Math.round(box.y), 0);
	const clip = { x: Math.round(box.x), y: top, width: Math.round(box.width), height: view.clipHeight };
	expect(clip.y + clip.height, `${view.id}: the main content ends above the clip`).toBeLessThanOrEqual(Math.round(box.y + box.height));
	for (const theme of THEMES) {
		await page.emulateMedia({ colorScheme: theme, reducedMotion: 'reduce' });
		await page.waitForTimeout(150);
		await assertNoLeaks(page, secrets, `${view.id} (${theme})`, clip);
		const png = await page.screenshot({ clip, animations: 'disabled', caret: 'hide', type: 'png' });
		const encoded = await encodeAll(page, png, clip, WIDTHS[view.viewport]);
		const files = encoded.map((e) => {
			const name = `${view.id}-${theme}-${e.width}.webp`;
			writeFileSync(`${out}/${name}`, e.data);
			return { name, width: e.width, height: e.height, bytes: e.bytes };
		});
		entries.push({ id: view.id, route: view.route, viewport: view.viewport, theme, files });
	}
	void server;
}

const byId = (id: string) => VIEWS.find((v) => v.id === id)!;

test.describe('desktop views', () => {
	test.use({ viewport: { width: 1040, height: 1000 }, deviceScaleFactor: 2 });

	test('deploy-live', async ({ page }) => {
		const server = await startServer();
		try {
			seedService(server, { project: DEMO.project, name: 'api', image: `${DEMO.registry}/api:1.4.0`, containerPort: 8080, deploys: [{ number: 6, live: true }, { number: 7, status: 'queued' }] });
			await mockDeployGet(page, () => LIVE_DEPLOY);
			const secrets = await open(page, server);
			await page.goto(`${server.baseURL}/services/${DEMO.project}/api/deploys/7`);
			await expect(page.getByTestId('stage-health')).toHaveAttribute('data-state', 'running');
			await capture(page, server, secrets, byId('deploy-live'));
		} finally {
			await server.stop();
		}
	});

	test('services', async ({ page }) => {
		const server = await startServer();
		try {
			await mockAction(page, 'services.list', SERVICES);
			const secrets = await open(page, server);
			await page.goto(`${server.baseURL}/services`);
			await expect(page.locator('[data-row]:visible')).toHaveCount(3);
			await capture(page, server, secrets, byId('services'));
		} finally {
			await server.stop();
		}
	});

	test('service-detail', async ({ page }) => {
		const server = await startServer();
		try {
			await mockAction(page, 'services.get', service('api'));
			await mockAction(page, 'deploys.list', HISTORY);
			const secrets = await open(page, server);
			await page.goto(`${server.baseURL}/services/${DEMO.project}/api`);
			await expect(page.locator('[data-row="5"]:visible')).toBeVisible();
			await capture(page, server, secrets, byId('service-detail'));
		} finally {
			await server.stop();
		}
	});

	test('rollback-dialog', async ({ page }) => {
		const server = await startServer();
		try {
			await mockAction(page, 'services.get', service('api'));
			await mockAction(page, 'deploys.list', HISTORY);
			const secrets = await open(page, server);
			await page.goto(`${server.baseURL}/services/${DEMO.project}/api`);
			await page.locator('[data-row="5"]:visible').getByRole('button', { name: 'Roll back to #5' }).click();
			const dialog = page.getByRole('dialog');
			await expect(dialog.getByRole('button', { name: 'Roll back to #5' })).toBeVisible();
			await capture(page, server, secrets, byId('rollback-dialog'));
		} finally {
			await server.stop();
		}
	});

	test('new-service', async ({ page }) => {
		const server = await startServer();
		try {
			const secrets = await open(page, server);
			await page.goto(`${server.baseURL}/services/new`);
			await expect(page.getByRole('heading', { name: 'Deploy a Docker image', level: 1 })).toBeVisible();
			await page.getByLabel('Project').fill(DEMO.project);
			await page.getByLabel('Name').fill('web');
			await page.getByLabel('Image').fill(`${DEMO.registry}/web:2.1.0`);
			await page.getByLabel('Port your app listens on (optional)').fill('8080');
			await capture(page, server, secrets, byId('new-service'));
		} finally {
			await server.stop();
		}
	});
});

test.describe('phone views', () => {
	test.use({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true });

	test('phone-deploy', async ({ page }) => {
		const server = await startServer();
		try {
			seedService(server, { project: DEMO.project, name: 'api', image: `${DEMO.registry}/api:1.4.0`, containerPort: 8080, deploys: [{ number: 6, live: true }, { number: 7, status: 'queued' }] });
			await mockDeployGet(page, () => LIVE_DEPLOY);
			const secrets = await open(page, server);
			await page.goto(`${server.baseURL}/services/${DEMO.project}/api/deploys/7`);
			await expect(page.getByTestId('stage-health')).toHaveAttribute('data-state', 'running');
			await capture(page, server, secrets, byId('phone-deploy'));
		} finally {
			await server.stop();
		}
	});
});

// ---- the capture's own assertions ------------------------------------------------------------------

test.describe('leak check', () => {
	const secrets: Secrets = { exact: ['planted-runtime-value-123'] };
	test('finds a planted runtime value, a token, an address, a host and an email', () => {
		expect(findLeaks('a planted-runtime-value-123 here', secrets)).not.toEqual([]);
		expect(findLeaks('sd_AbCdEfGhIjKlMnOpQr', secrets)).not.toEqual([]);
		expect(findLeaks('http://127.0.0.1:8080', secrets)).not.toEqual([]);
		expect(findLeaks('open box.internal-corp.com', secrets)).not.toEqual([]);
		expect(findLeaks('mail me@corp.io', secrets)).not.toEqual([]);
		expect(findLeaks('0123456789abcdef0123456789abcdef0123456789abcdefXYZ', secrets)).not.toEqual([]);
	});
	test('passes demo data and the allowlisted digest', () => {
		expect(findLeaks('registry.example.test/demo/api:1.4.0 on api.example.test, deploy #7', secrets)).toEqual([]);
		expect(findLeaks(`digest ${DEMO.digests[0]}`, secrets)).toEqual([]);
	});
	test('a page with a planted runtime value or a planted form value fails the check', async ({ page }) => {
		const box = { x: 0, y: 0, width: 800, height: 400 };
		await page.setContent('<main><p>a planted-runtime-value-123 here</p></main>');
		await expect(assertNoLeaks(page, secrets, 'planted text', box)).rejects.toThrow(/would show/);
		await page.setContent('<main><label>Name <input value="planted-runtime-value-123"></label></main>');
		await expect(assertNoLeaks(page, secrets, 'planted value', box)).rejects.toThrow(/would show/);
		await page.setContent('<main><p>registry.example.test/demo/web:2.1.0</p></main>');
		await assertNoLeaks(page, secrets, 'clean page', box);
		await page.setContent('<main><p style="margin-top:900px">planted-runtime-value-123</p></main>');
		await assertNoLeaks(page, secrets, 'text below the clip is not in the image', box);
	});
	test('the output directory refuses site/public/screens', () => {
		const before = process.env.SCREENS_OUT;
		process.env.SCREENS_OUT = resolve('..', 'site', 'public', 'screens');
		expect(() => outDir()).toThrow(/must not be/);
		process.env.SCREENS_OUT = before;
	});
});
