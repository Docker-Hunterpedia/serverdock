import type { Page } from '@playwright/test';
import { countApiRequests, expect, expectNoAxeViolations, letTheAccountAge, signIn, test } from './fixtures';
import { mockExposure, mockServices, seedService, seedThreeServices, serviceFixture } from './seed';
import { DOCKER_SOCKET, TEST_APP_IMAGE, startServer, uniqueProject } from './server';

// Plan 0004 step 12: Services list, New service, Overview. The implementer's smoke
// tests; the tester extends them to every listed screen state in all 8 projects.
test.use({ storageState: { cookies: [], origins: [] } });

test('Overview and Services are empty on a new server', async ({ page, server, admin }) => {
	await signIn(page, server, admin);
	await page.goto(`${server.baseURL}/`);
	await expect(page.getByRole('heading', { name: 'No services yet' })).toBeVisible();
	await expect(page.getByRole('link', { name: 'Deploy an image' })).toBeVisible();
	await expectNoAxeViolations(page, 'Overview, empty');
	if ((page.viewportSize()?.width ?? 1280) < 768) await page.getByRole('button', { name: 'Menu' }).click();
	await page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: 'Services' }).click();
	await expect(page.getByRole('heading', { name: 'Services', level: 1 })).toBeVisible();
	await expect(page.getByRole('heading', { name: 'No services yet' })).toBeVisible();
	await expectNoAxeViolations(page, 'Services, empty');
});

test('seeded services are grouped by project and show unknown without Docker', async ({ page, server, admin }) => {
	seedThreeServices(server);
	await signIn(page, server, admin);
	await page.goto(`${server.baseURL}/services`);
	await expect(page.getByRole('heading', { name: 'shop', level: 2 })).toBeVisible();
	await expect(page.getByRole('heading', { name: 'blog', level: 2 })).toBeVisible();
	const api = page.locator('[data-row="shop/api"]:visible');
	await expect(api).toContainText('Unknown'); // no Docker socket: the events stream is down
	await expect(page.locator('[data-row="blog/web"]:visible')).toContainText('Not deployed');
	await expectNoAxeViolations(page, 'Services, seeded');
});

test('Overview lists what needs attention and sends nothing while idle', async ({ page, server, admin }) => {
	seedService(server, { project: 'shop', name: 'api', needsAttention: 'previous_version_down', deploys: [{ number: 1, status: 'failed', appSafe: 'no', live: false }] });
	await signIn(page, server, admin);
	await page.goto(`${server.baseURL}/`);
	const list = page.getByTestId('needs-attention');
	await expect(list).toContainText('shop/api');
	await expect(list).toContainText('Last deploy #1 failed');
	await expect(list).toContainText('previous version did not come back');
	const reqs = countApiRequests(page);
	await page.waitForTimeout(3000);
	expect(reqs.seen.filter((p) => !p.endsWith('/actions/system.info'))).toEqual([]);
});

test('New service validates, keeps typed input and deploys the test app for real', async ({ page, allow5xx }) => {
	test.setTimeout(240_000); // a real pull, start and health gate
	allow5xx(); // a fresh server with Docker may log a 503 on the first reads while it settles
	const server = await startServer({ dockerSocket: DOCKER_SOCKET, allowLoopbackRegistry: true });
	try {
		const admin = await server.firstRun();
		await letTheAccountAge();
		await signIn(page, server, admin);
		await page.goto(`${server.baseURL}/services/new`);
		await expect(page.getByRole('heading', { name: 'Deploy a Docker image' })).toBeVisible();
		await page.getByLabel('Name').fill('Bad Name');
		await page.getByRole('button', { name: 'Create and deploy' }).click();
		await expect(page.getByRole('alert')).toContainText('fields need fixing');
		await expect(page.getByLabel('Name')).toHaveValue('Bad Name');
		const project = uniqueProject();
		await page.getByLabel('Project').fill(project);
		await page.getByLabel('Name').fill('web');
		await page.getByLabel('Image').fill(TEST_APP_IMAGE);
		await page.getByLabel('Port your app listens on (optional)').fill('8080');
		await page.getByRole('button', { name: 'Create and deploy' }).click();
		await expect(page).toHaveURL(new RegExp(`/services/${project}/web/deploys/1$`));
		await expect(page.getByRole('heading', { name: 'Deploy #1' })).toBeVisible();
		await expect
			.poll(() => server.query<{ status: string; reason: string | null }>(`SELECT status, reason FROM deploys WHERE number = 1`).map((r) => (r.status === 'failed' ? `failed: ${r.reason}` : r.status))[0], { timeout: 180_000 })
			.toBe('succeeded');
		await page.goto(`${server.baseURL}/services`);
		const row = page.locator(`[data-row="${project}/web"]:visible`);
		await expect(row).toContainText('Running');
		await expect(row).toContainText('●');
		await expect(row).toContainText('#1');
		await expectNoAxeViolations(page, 'Services, a real running service');
		await page.goto(`${server.baseURL}/`);
		await expect(page.getByTestId('state-counts').locator('li[data-state="running"]')).toContainText('1');
		await expect(page.getByRole('table', { name: /latest deploy/ }).or(page.locator('ul.cards:visible'))).toContainText(`${project}/web #1`);
		await expect(page.getByTestId('needs-attention')).toHaveCount(0);
		await expectNoAxeViolations(page, 'Overview, a real running service');
	} finally {
		await server.stop();
	}
});


// ---- plan 0004 step 12, the tester's additions ----------------------------------------

const isPhone = (page: Page) => (page.viewportSize()?.width ?? 1280) < 768;
const tab = (page: Page) => page.keyboard.press('Tab');
const MOCK_FOUR = () => [
	serviceFixture({ project: 'shop', name: 'api', state: 'running', public_port: 8081, active_deploy: 2 }),
	serviceFixture({
		project: 'shop',
		name: 'worker',
		state: 'exited',
		container_port: null,
		last_deploy: { number: 3, kind: 'deploy', status: 'failed', created_at: '2026-11-21T08:30:00.000Z', finished_at: '2026-11-21T08:31:00.000Z', app_safe: 'yes', image_ref: 'registry.example.com/shop/worker:2.4.1' }
	}),
	serviceFixture({ project: 'blog', name: 'web', state: 'missing', image_ref: 'ghost:5' }),
	serviceFixture({ project: 'blog', name: 'cache', state: 'unknown', image_ref: 'redis:7' })
];

async function openNew(page: Page, server: { baseURL: string }) {
	await page.goto(`${server.baseURL}/services/new`);
	await expect(page.getByRole('heading', { name: 'Deploy a Docker image', level: 1 })).toBeVisible();
}

test.describe('Services list screen states', () => {
	test('three seeded services in two projects: table or cards, no axe violations, captions and rows', async ({ page, server, admin }) => {
		seedThreeServices(server);
		await signIn(page, server, admin);
		await page.goto(`${server.baseURL}/services`);
		await expect(page.getByRole('heading', { name: 'shop', level: 2 })).toBeVisible();
		await expect(page.locator('[data-row]:visible')).toHaveCount(3);
		if (isPhone(page)) {
			// below 768 px the cards show and the table is hidden
			await expect(page.locator('table:visible')).toHaveCount(0);
		} else {
			await expect(page.getByRole('table', { name: 'Services in shop' })).toBeVisible();
			await expect(page.getByRole('table', { name: 'Services in blog' })).toBeVisible();
		}
		await expect(page.locator('[data-row="shop/worker"]:visible')).toContainText(/#1.*Failed/s);
		await expectNoAxeViolations(page, 'Services, 3 seeded services');
	});

	test('running, exited, missing and unknown show a symbol plus a word, no axe violations', async ({ page, server, admin }) => {
		await mockServices(page, MOCK_FOUR());
		await signIn(page, server, admin);
		await page.goto(`${server.baseURL}/services`);
		const expectState = async (row: string, symbol: string, word: string) => {
			const r = page.locator(`[data-row="${row}"]:visible`);
			await expect(r, row).toContainText(word);
			await expect(r, row).toContainText(symbol);
		};
		await expectState('shop/api', '●', 'Running');
		await expectState('shop/worker', '▲', 'Exited');
		await expectState('blog/web', '◆', 'Missing');
		await expectState('blog/cache', '◌', 'Unknown');
		await expect(page.locator('[data-row="shop/api"]:visible')).toContainText(':8081');
		await expect(page.locator('[data-row="shop/worker"]:visible')).toContainText('#3');
		// the symbol is decoration: the accessible text carries the word
		await expect(page.locator('[data-row="blog/web"]:visible').getByText('◆')).toHaveAttribute('aria-hidden', 'true');
		await expectNoAxeViolations(page, 'Services, four states');
	});

	test('a real Docker socket makes a seeded service with no container "Missing"', async ({ page, allow5xx }) => {
		allow5xx();
		const server = await startServer({ dockerSocket: DOCKER_SOCKET });
		try {
			const admin = await server.firstRun();
			seedThreeServices(server);
			await signIn(page, server, admin);
			await page.goto(`${server.baseURL}/services`);
			const row = page.locator('[data-row="shop/api"]:visible');
			await expect(async () => {
				await page.getByRole('button', { name: 'Refresh' }).click();
				await expect(row).toContainText('Missing', { timeout: 2000 });
			}).toPass({ timeout: 30_000 });
			await expect(row).toContainText('◆');
			await expectNoAxeViolations(page, 'Services, a missing container');
		} finally {
			await server.stop();
		}
	});

	test('the list asks the server again only on Refresh, and Refresh shows a new service', async ({ page, server, admin }) => {
		seedThreeServices(server);
		await signIn(page, server, admin);
		await page.goto(`${server.baseURL}/services`);
		await expect(page.locator('[data-row]:visible')).toHaveCount(3);
		const reqs = countApiRequests(page);
		seedService(server, { project: 'blog', name: 'docs', image: 'nginx:1.27' });
		await page.getByRole('button', { name: 'Refresh' }).click();
		await expect(page.locator('[data-row="blog/docs"]:visible')).toBeVisible();
		await expect(page.getByRole('status').filter({ hasText: 'Updated. 4 services.' })).toBeVisible();
		expect(reqs.seen.filter((p) => p.endsWith('/actions/services.list')).length).toBe(1);
	});

	test('a failed load says the status is unknown and Refresh recovers', async ({ page, server, admin, allow5xx }) => {
		allow5xx();
		seedThreeServices(server);
		await signIn(page, server, admin);
		let fail = true;
		await page.route('**/api/v1/actions/services.list', (route) => (fail ? route.fulfill({ status: 500, contentType: 'application/json', body: '{"error":{"code":"internal","message":"boom"}}' }) : route.continue()));
		await page.goto(`${server.baseURL}/services`);
		await expect(page.getByRole('alert')).toBeVisible();
		await expect(page.getByText('Status unknown: could not load the services')).toBeVisible();
		await expect(page.locator('[data-row]')).toHaveCount(0); // never a stale list
		await expectNoAxeViolations(page, 'Services, load failed');
		fail = false;
		await page.getByRole('button', { name: 'Refresh' }).click();
		await expect(page.locator('[data-row]:visible')).toHaveCount(3);
	});
});

test.describe('New service screen states', () => {
	test('empty form, no axe violations', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		await openNew(page, server);
		await expect(page.getByLabel('Project')).toHaveValue('default');
		await expectNoAxeViolations(page, 'New service, empty');
	});

	test('field errors and the error summary: focus, links to the fields, no axe violations', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		await openNew(page, server);
		await page.getByLabel('Project').fill('Bad Project');
		await page.getByLabel('Name').fill('Bad Name');
		await page.getByLabel('Public port (optional)').fill('80');
		await page.getByRole('button', { name: 'Create and deploy' }).click();
		const alert = page.getByRole('alert').filter({ hasText: 'need fixing' });
		await expect(alert).toBeFocused();
		await expect(alert).toContainText('4 fields need fixing');
		// every error has text, a symbol and a link from the summary
		for (const [id, label] of [['f-project', 'Project'], ['f-name', 'Name'], ['f-image', 'Image'], ['f-public-port', 'Public port']] as const) {
			await expect(page.locator(`#${id}`)).toHaveAttribute('aria-invalid', 'true');
			await expect(page.locator(`#${id}-error`)).toBeVisible();
			await expect(alert.locator(`a[href="#${id}"]`)).toContainText(label);
		}
		await expectNoAxeViolations(page, 'New service, field errors and summary');
		await alert.locator('a[href="#f-image"]').click();
		await expect(page.locator('#f-image')).toBeFocused();
	});

	test('public-port hints: visible, linked to the field, with the docs link', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		await openNew(page, server);
		const field = page.locator('#f-public-port');
		await expect(field).toHaveAttribute('aria-describedby', /\bf-public-port-hint\b/);
		// the long SEC6 paragraph is part of the description only once a public port is set
		await expect(field).not.toHaveAttribute('aria-describedby', /\bpublic-sec6\b/);
		await expect(field).not.toHaveAccessibleDescription(/Read about this/);
		await expect(page.locator('#f-public-port-hint')).not.toContainText('<port>');
		await field.fill('8080');
		await expect(field).toHaveAttribute('aria-describedby', /\bpublic-sec6\b/);
		await expect(page.locator('#f-public-port-hint')).toContainText("Exposes the app on the internet at http://");
		await expect(page.locator('#f-public-port-hint')).toContainText(':8080');
		await expect(page.locator('#f-public-port-hint')).toContainText("unless your host's firewall blocks the port");
		const sec6 = page.locator('#public-sec6');
		await expect(sec6).toBeVisible();
		const docs = sec6.getByRole('link', { name: /Read about this/ });
		await expect(docs).toHaveAttribute('href', /^https:\/\/github\.com\//);
		// what a screen reader gets as the field's description
		await expect(field).toHaveAccessibleDescription(/firewall[\s\S]*Read about this/);
		await expect(page.getByText('After the deploy the app will be at')).toBeVisible();
		await expectNoAxeViolations(page, 'New service, public-port hints');
	});

	test('suggested port: linked to its field, "Use N" fills it', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		await openNew(page, server);
		await page.getByLabel('Image').fill('nginx:1.27');
		const sug = page.locator('#port-suggestion');
		await expect(sug).toContainText('Suggested for nginx: port 80');
		await expect(page.locator('#f-container-port')).toHaveAttribute('aria-describedby', /\bport-suggestion\b/);
		await expect(page.locator('#f-container-port')).toHaveAccessibleDescription(/Suggested for nginx: port 80/);
		await expectNoAxeViolations(page, 'New service, suggested port');
		await sug.getByRole('button', { name: 'Use port 80' }).click();
		await expect(page.locator('#f-container-port')).toHaveValue('80');
		// the button removes itself: focus goes to the field and the change is announced
		await expect(page.locator('#f-container-port')).toBeFocused();
		await expect(page.getByRole('status')).toContainText('Port set to 80.');
		await expect(sug.getByRole('button')).toHaveCount(0);
		await page.getByLabel('Image').fill('unknown-thing:1');
		await expect(sug).toHaveCount(0);
		await expect(page.locator('#f-container-port')).not.toHaveAttribute('aria-describedby', /port-suggestion/);
	});

	test('Advanced open, and an error inside it opens it', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		await openNew(page, server);
		const details = page.locator('details');
		await expect(details).not.toHaveAttribute('open', '');
		await page.getByText('Advanced', { exact: true }).click();
		await expect(details).toHaveAttribute('open', '');
		for (const id of ['f-timeout', 'f-passes', 'f-drain', 'f-grace']) await expect(page.locator(`#${id}`)).toBeVisible();
		await expectNoAxeViolations(page, 'New service, Advanced open');
		await page.getByLabel('Name').fill('web');
		await page.getByLabel('Image').fill('nginx:1.27');
		await page.getByLabel('Health timeout (seconds)').fill('2');
		// closed again: the summary must still reach the bad field, so the error opens it
		await page.getByText('Advanced', { exact: true }).click();
		await expect(page.locator('#f-timeout')).toBeHidden();
		await page.getByRole('button', { name: 'Create and deploy' }).click();
		await expect(details).toHaveAttribute('open', '');
		await expect(page.locator('#f-timeout-error')).toContainText('5 to 600');
		await expectNoAxeViolations(page, 'New service, Advanced error');
	});

	test('typed input is kept on a browser-side error and on a server-side one', async ({ page, server, admin }) => {
		seedService(server, { project: 'shop', name: 'api', containerPort: 8080 });
		await signIn(page, server, admin);
		await openNew(page, server);
		await page.getByLabel('Project').fill('shop');
		await page.getByLabel('Name').fill('Bad Name');
		await page.getByLabel('Image').fill('registry.example.com/shop/api:2.4.1');
		await page.getByLabel('Port your app listens on (optional)').fill('8080');
		await page.getByLabel('Public port (optional)').fill('8081');
		await page.getByRole('radio', { name: /^HTTP/ }).check();
		await page.getByLabel('Health check path').fill('/up');
		await page.getByRole('radio', { name: /Stop-first/ }).check();
		await page.getByText('Advanced', { exact: true }).click();
		await page.getByLabel('Health passes').fill('4');
		await page.getByLabel('Drain time (seconds)').fill('30');
		const kept = async (name: string) => {
			await expect(page.getByLabel('Project')).toHaveValue('shop');
			await expect(page.getByLabel('Name')).toHaveValue(name);
			await expect(page.getByLabel('Image')).toHaveValue('registry.example.com/shop/api:2.4.1');
			await expect(page.getByLabel('Port your app listens on (optional)')).toHaveValue('8080');
			await expect(page.getByLabel('Public port (optional)')).toHaveValue('8081');
			await expect(page.getByRole('radio', { name: /^HTTP/ })).toBeChecked();
			await expect(page.getByLabel('Health check path')).toHaveValue('/up');
			await expect(page.getByRole('radio', { name: /Stop-first/ })).toBeChecked();
			await expect(page.getByLabel('Health passes')).toHaveValue('4');
			await expect(page.getByLabel('Drain time (seconds)')).toHaveValue('30');
		};
		await page.getByRole('button', { name: 'Create and deploy' }).click();
		await expect(page.locator('#f-name-error')).toBeVisible();
		await kept('Bad Name');
		// the server's own answer: a name that already exists
		await page.getByLabel('Name').fill('api');
		await page.getByRole('button', { name: 'Create and deploy' }).click();
		// with a public port the create waits up to 10 s for the routes to be ready (no Docker here), so say so and wait
		await expect(page.getByRole('status')).toContainText('Creating the service');
		await expect(page.getByRole('alert').filter({ hasText: /already exists/i })).toBeVisible({ timeout: 25_000 });
		await expect(page).toHaveURL(/\/services\/new$/);
		await kept('api');
		expect(server.query(`SELECT 1 FROM deploys`).length).toBe(0); // nothing was queued
		await expectNoAxeViolations(page, 'New service, server error');
	});

	test('a server-side failure of the call itself keeps the input too', async ({ page, server, admin, allow5xx }) => {
		allow5xx();
		await signIn(page, server, admin);
		await page.route('**/api/v1/actions/services.create', (route) => route.abort('connectionreset'));
		await openNew(page, server);
		await page.getByLabel('Name').fill('web');
		await page.getByLabel('Image').fill('nginx:1.27');
		await page.getByRole('button', { name: 'Create and deploy' }).click();
		await expect(page.getByRole('alert').filter({ hasText: /./ })).toBeVisible();
		await expect(page.getByLabel('Name')).toHaveValue('web');
		await expect(page.getByLabel('Image')).toHaveValue('nginx:1.27');
		await expect(page).toHaveURL(/\/services\/new$/);
		// never claims success
		await expect(page.getByRole('status')).not.toContainText(/created|queued/i);
	});
});

test.describe('Overview screen states', () => {
	test('with services: counts per state, recent deploys, no axe violations', async ({ page, server, admin }) => {
		await mockServices(page, MOCK_FOUR());
		await mockExposure(page, []);
		await signIn(page, server, admin);
		await page.goto(`${server.baseURL}/`);
		const counts = page.getByTestId('state-counts');
		await expect(counts).toBeVisible();
		for (const [st, n] of [['running', '1'], ['exited', '1'], ['missing', '1'], ['unknown', '1'], ['restarting', '0']] as const) {
			await expect(counts.locator(`li[data-state="${st}"]`), st).toContainText(n);
		}
		await expect(page.getByRole('heading', { name: 'Recent deploys' })).toBeVisible();
		await expect(page.getByTestId('exposure-banner')).toHaveCount(0);
		await expectNoAxeViolations(page, 'Overview, with services');
	});

	test('the needs-attention list names each reason with a symbol', async ({ page, server, admin }) => {
		await mockServices(page, MOCK_FOUR());
		await mockExposure(page, []);
		await signIn(page, server, admin);
		await page.goto(`${server.baseURL}/`);
		const list = page.getByTestId('needs-attention');
		await expect(list).toContainText('shop/worker');
		await expect(list).toContainText('Last deploy #3 failed (the previous version kept serving)');
		await expect(list).toContainText('The live version has exited');
		await expect(list).toContainText('blog/web');
		await expect(list).toContainText('container is missing');
		await expect(list).toContainText('cannot confirm this service');
		await expect(list).not.toContainText('shop/api');
		await expectNoAxeViolations(page, 'Overview, needs attention');
	});

	test('the exposure banner lists the containers and links the docs, no axe violations', async ({ page, server, admin }) => {
		await mockServices(page, MOCK_FOUR());
		await mockExposure(page, [{ name: 'grafana', ports: [[3000, 'tcp']] }, { name: 'redis', ports: [[6379, 'tcp']] }]);
		await signIn(page, server, admin);
		await page.goto(`${server.baseURL}/`);
		const banner = page.getByTestId('exposure-banner');
		await expect(banner).toBeVisible();
		await expect(banner.getByRole('heading', { level: 2 })).toContainText('Other containers use this server');
		await expect(banner).toContainText('grafana (3000/tcp)'); // the HOST port, not the container's (mock: 80)
		await expect(banner).not.toContainText('(80/tcp)');
		await expect(banner).toContainText('redis (6379/tcp)');
		await expect(banner.getByRole('link', { name: /Read about this/ })).toHaveAttribute('href', /^https:\/\/github\.com\//);
		await expectNoAxeViolations(page, 'Overview, exposure banner');
	});

	test('the banner also shows on an empty server, and a failed exposure check is said honestly', async ({ page, server, admin, allow5xx }) => {
		await mockExposure(page, [{ name: 'grafana', ports: [[3000, 'tcp']] }]);
		await signIn(page, server, admin);
		await page.goto(`${server.baseURL}/`);
		await expect(page.getByTestId('exposure-banner')).toBeVisible();
		await expect(page.getByRole('heading', { name: 'No services yet' })).toBeVisible();
		await expectNoAxeViolations(page, 'Overview, empty with the banner');
		allow5xx();
		await page.unroute('**/api/v1/actions/system.exposure');
		await page.route('**/api/v1/actions/system.exposure', (r) => r.fulfill({ status: 503, contentType: 'application/json', body: '{"error":{"code":"unavailable","message":"no docker"}}' }));
		await page.getByRole('button', { name: 'Refresh' }).click();
		await expect(page.locator('p.note', { hasText: 'Could not check which other containers publish ports' })).toBeVisible();
		await expect(page.getByRole('status')).toContainText('Could not check which other containers publish ports'); // the Refresh result says it too
		await expect(page.getByTestId('exposure-banner')).toHaveCount(0);
	});

	test('Refresh reloads the overview', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		await page.goto(`${server.baseURL}/`);
		await expect(page.getByRole('heading', { name: 'No services yet' })).toBeVisible();
		seedService(server, { project: 'shop', name: 'api', needsAttention: 'delete_interrupted' });
		const reqs = countApiRequests(page);
		await page.getByRole('button', { name: 'Refresh' }).click();
		await expect(page.getByTestId('needs-attention')).toContainText('A delete did not finish');
		await expect(page.getByRole('status').filter({ hasText: 'Updated.' })).toBeVisible();
		expect(reqs.seen.filter((p) => p.endsWith('/actions/services.list')).length).toBe(1);
		expect(reqs.seen.filter((p) => p.endsWith('/actions/system.exposure')).length).toBe(1);
	});
});

test.describe('idle', () => {
	// A real 30 s window, not a fake clock: a timer that survives would be missed by a
	// stubbed one. The Services list and the Overview run in two tabs of one context at
	// the same time, so a project costs 30 s, not 60 s.
	test('the Services list and Overview send no requests for 30 s', async ({ page, context, server, admin }) => {
		test.setTimeout(90_000);
		seedThreeServices(server);
		await signIn(page, server, admin);
		const other = await context.newPage();
		await page.goto(`${server.baseURL}/services`);
		await other.goto(`${server.baseURL}/`);
		await expect(page.locator('[data-row]:visible')).toHaveCount(3);
		await expect(other.getByTestId('state-counts')).toBeVisible();
		await page.waitForLoadState('networkidle');
		const a = countApiRequests(page);
		const b = countApiRequests(other);
		const t0 = Date.now();
		await page.waitForTimeout(30_000);
		const secs = (Date.now() - t0) / 1000;
		console.log(`idle window ${secs.toFixed(1)} s: Services ${a.seen.length} requests, Overview ${b.seen.length} requests`);
		expect(secs).toBeGreaterThanOrEqual(30);
		// The shell's footer status polls system.info every 15 s (src/lib/status.ts, M2, visible
		// pages only). It is not the page's own traffic: the pages send nothing else.
		const own = (seen: string[]) => seen.filter((p) => !p.endsWith('/actions/system.info'));
		expect(own(a.seen), 'Services list requests while idle').toEqual([]);
		expect(own(b.seen), 'Overview requests while idle').toEqual([]);
		expect(a.seen.length, 'footer status polls only').toBeLessThanOrEqual(3);
		// and the pages still work afterwards: Refresh is the one way to ask again
		await page.getByRole('button', { name: 'Refresh' }).click();
		await expect.poll(() => a.seen.length).toBeGreaterThan(0);
		await other.close();
	});
});

test.describe('keyboard and targets', () => {
	test('keyboard only: Services, New service, a failed submit, the summary link, the fix', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		await page.goto(`${server.baseURL}/services`);
		await expect(page.getByRole('heading', { name: 'Services', level: 1 })).toBeVisible();
		const tabTo = async (target: ReturnType<Page['locator']>, what: string) => {
			for (let i = 0; i < 40; i++) {
				if (await target.evaluate((el) => el === document.activeElement).catch(() => false)) return;
				await tab(page);
			}
			throw new Error(`Tab never reached ${what}`);
		};
		await tabTo(page.getByRole('main').getByRole('link', { name: 'New service' }), 'New service');
		await page.keyboard.press('Enter');
		await expect(page).toHaveURL(/\/services\/new$/);
		await expect(page.getByRole('heading', { name: 'Deploy a Docker image', level: 1 })).toBeFocused();
		// Tab order follows the visual order of the fields
		const order = ['f-project', 'f-name', 'f-image', 'f-container-port', 'f-public-port'];
		for (const id of order) {
			await tab(page);
			await expect(page.locator(`#${id}`), id).toBeFocused();
		}
		// type with the keyboard; leave the name invalid
		await page.locator('#f-name').focus();
		await page.keyboard.type('Bad');
		await tab(page);
		await page.keyboard.type('nginx:1.27');
		// the suggestion button is the next stop after the port field
		await tab(page);
		await expect(page.locator('#f-container-port')).toBeFocused();
		await tab(page);
		await expect(page.getByRole('button', { name: 'Use port 80' })).toBeFocused();
		await page.keyboard.press('Enter');
		await expect(page.locator('#f-container-port')).toHaveValue('80');
		// radios, Advanced (opens with Space), then submit
		await tabTo(page.getByRole('radio', { name: /^TCP/ }), 'the health radios');
		await page.keyboard.press('ArrowDown');
		await expect(page.getByRole('radio', { name: /^HTTP/ })).toBeChecked();
		await page.keyboard.press('ArrowDown');
		await expect(page.getByRole('radio', { name: /^Stays running/ })).toBeChecked();
		await tabTo(page.getByText('Advanced', { exact: true }), 'Advanced');
		await page.keyboard.press('Space');
		await expect(page.locator('#f-timeout')).toBeVisible();
		await tabTo(page.getByRole('button', { name: 'Create and deploy' }), 'Create and deploy');
		await page.keyboard.press('Enter');
		const alert = page.getByRole('alert').filter({ hasText: 'needs fixing' });
		await expect(alert).toBeFocused();
		await tab(page); // the first link in the summary is the Name field
		await expect(alert.getByRole('link', { name: /^Name/ })).toBeFocused();
		await page.keyboard.press('Enter');
		await expect(page.locator('#f-name')).toBeFocused();
		await expect(page.locator('#f-name')).toHaveValue('Bad');
		// no focus trap: Shift+Tab walks back out
		await page.keyboard.press('Shift+Tab');
		await expect(page.locator('#f-project')).toBeFocused();
		await expectNoAxeViolations(page, 'New service, keyboard path');
	});

	test('interactive targets are at least 44 px', async ({ page, server, admin }) => {
		seedThreeServices(server);
		await signIn(page, server, admin);
		const big = async (loc: ReturnType<Page['locator']>, what: string) => {
			const box = await loc.first().boundingBox();
			expect(box, what).not.toBeNull();
			expect(box!.height, `${what} height`).toBeGreaterThanOrEqual(44);
			expect(box!.width, `${what} width`).toBeGreaterThanOrEqual(44);
		};
		await page.goto(`${server.baseURL}/services`);
		await expect(page.locator('[data-row]:visible')).toHaveCount(3);
		await big(page.getByRole('main').getByRole('link', { name: 'New service' }), 'New service');
		await big(page.getByRole('button', { name: 'Refresh' }), 'Refresh');
		await big(page.locator('[data-row="shop/api"]:visible').getByRole('link', { name: 'api' }), 'service name link');
		await page.goto(`${server.baseURL}/`);
		await expect(page.getByTestId('state-counts')).toBeVisible();
		await big(page.getByRole('main').getByRole('link', { name: 'New service' }), 'Overview New service');
		await big(page.getByRole('button', { name: 'Refresh' }), 'Overview Refresh');
		await big(page.getByRole('link', { name: 'All services' }), 'All services');
		await page.goto(`${server.baseURL}/services/new`);
		await page.getByLabel('Image').fill('nginx:1.27');
		for (const id of ['f-project', 'f-name', 'f-image', 'f-container-port', 'f-public-port']) await big(page.locator(`#${id}`), id);
		await big(page.getByRole('button', { name: 'Use port 80' }), 'Use port 80');
		await big(page.getByRole('button', { name: 'Create and deploy' }), 'Create and deploy');
		await big(page.getByRole('link', { name: 'Cancel' }), 'Cancel');
		await big(page.getByText('Advanced', { exact: true }), 'Advanced summary');
		for (const re of [/TCP:/, /HTTP:/, /Stays running/, /Blue-green/, /Stop-first/]) await big(page.locator('.choice').filter({ hasText: re }), `radio ${re}`);
		await page.getByText('Advanced', { exact: true }).click();
		for (const id of ['f-timeout', 'f-passes', 'f-drain', 'f-grace']) await big(page.locator(`#${id}`), id);
	});

	test('the pages send a Content-Security-Policy and load without a violation', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		for (const path of ['/', '/services', '/services/new']) {
			const res = await page.goto(`${server.baseURL}${path}`);
			expect(res!.headers()['content-security-policy'], path).toMatch(/default-src|script-src/);
			await expect(page.getByRole('heading', { level: 1 })).toBeVisible();
		}
		// The auto `problems` fixture turns every securitypolicyviolation event, page error and
		// console error of the three pages above into a failure of this test.
	});
});


test('a deploy that fails shows on the list and the Overview, not as success', async ({ page, allow5xx }) => {
	test.setTimeout(240_000);
	allow5xx();
	const server = await startServer({ dockerSocket: DOCKER_SOCKET, allowLoopbackRegistry: true });
	try {
		const admin = await server.firstRun();
		await letTheAccountAge();
		await signIn(page, server, admin);
		const project = uniqueProject();
		await page.goto(`${server.baseURL}/services/new`);
		await page.getByLabel('Project').fill(project);
		await page.getByLabel('Name').fill('web');
		await page.getByLabel('Image').fill('127.0.0.1:5000/sd-no-such-image:1');
		await page.getByRole('button', { name: 'Create and deploy' }).click();
		await expect(page).toHaveURL(new RegExp(`/services/${project}/web/deploys/1$`));
		await expect
			.poll(() => server.query<{ status: string }>(`SELECT status FROM deploys WHERE number = 1`)[0]?.status, { timeout: 180_000 })
			.toBe('failed');
		await page.goto(`${server.baseURL}/services`);
		const row = page.locator(`[data-row="${project}/web"]:visible`);
		await expect(row).toContainText('Failed');
		await expect(row).not.toContainText('Running');
		await expectNoAxeViolations(page, 'Services, a failed first deploy');
		await page.goto(`${server.baseURL}/`);
		await expect(page.getByTestId('needs-attention')).toContainText(`${project}/web`);
		await expect(page.getByTestId('needs-attention')).toContainText('Last deploy #1 failed');
		await expectNoAxeViolations(page, 'Overview, a real failed deploy');
	} finally {
		await server.stop();
	}
});


// ---- step 12 review fixes ----------------------------------------------------------

test.describe('review fixes', () => {
	test('the exposure banner shows host ports once, even when 0.0.0.0 and :: both publish', async ({ page, server, admin }) => {
		await mockExposure(page, [{ name: 'grafana', ports: [[3000, 'tcp', 80, '0.0.0.0'], [3000, 'tcp', 80, '::']] }]);
		await signIn(page, server, admin);
		await page.goto(`${server.baseURL}/`);
		const banner = page.getByTestId('exposure-banner');
		await expect(banner).toContainText('grafana (3000/tcp)');
		await expect(banner).not.toContainText('3000/tcp, 3000/tcp');
		await expect(banner).not.toContainText('80/tcp');
	});

	test('a create the proxy could not apply says so on the landing page, and the SEC6 text matches the server', async ({ page, server, admin }) => {
		// A server with no Docker: the route cannot be applied yet, and the first deploy fails later.
		await signIn(page, server, admin);
		await page.goto(`${server.baseURL}/services/new`);
		await page.getByLabel('Name').fill('web');
		await page.getByLabel('Image').fill('nginx:1.27');
		await page.getByLabel('Port your app listens on (optional)').fill('80');
		await page.getByLabel('Public port (optional)').fill('18090');
		const draft = (await page.locator('#public-sec6').innerText()).replace(/\s+/g, ' ');
		const created = page.waitForResponse((r) => r.url().endsWith('/actions/services.create'));
		await page.getByRole('button', { name: 'Create and deploy' }).click();
		const body = await (await created).json();
		const serverText = (body.data.service.warnings[0].message as string).replace(/\s+/g, ' ');
		expect(draft, 'the SPA draft starts with the server wording for the same port').toContain(serverText);
		expect(body.data.applied).toBe(false);
		await expect(page).toHaveURL(/\/services\/default\/web\/deploys\/1$/);
		const note = page.getByTestId('not-applied');
		await expect(note).toHaveText(`▲ ${body.data.notice}`.replace(/\s+/g, ' '));
		await expect(note).not.toContainText('Saved, but not applied yet: Saved');
		await expect(page.getByText('This deploy was just queued')).toBeVisible();
		await expectNoAxeViolations(page, 'Deploy placeholder, not applied');
	});

	test('a deploy page opened directly does not claim the deploy was just queued', async ({ page, server, admin }) => {
		seedService(server, { project: 'shop', name: 'api', deploys: [{ number: 1, status: 'succeeded', live: true }] });
		await signIn(page, server, admin);
		await page.goto(`${server.baseURL}/services/shop/api/deploys/1`);
		await expect(page.getByRole('heading', { name: 'Deploy #1' })).toBeVisible();
		await expect(page.getByText('just queued')).toHaveCount(0);
		await expect(page.getByText("does not show a deploy's status yet")).toBeVisible();
		await expect(page.getByTestId('not-applied')).toHaveCount(0);
	});

	test('a network failure on create does not claim nothing changed; typing clears a field error', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		await page.goto(`${server.baseURL}/services/new`);
		await page.route('**/actions/services.create', (r) => r.abort('failed'));
		await page.getByLabel('Name').fill('web');
		await page.getByLabel('Image').fill('nginx:1.27');
		await page.getByRole('button', { name: 'Create and deploy' }).click();
		await expect(page.getByRole('alert')).toContainText('may have been created');
		await expect(page.getByRole('alert')).not.toContainText('Nothing was changed');
		await page.getByLabel('Name').fill('Bad Name');
		await page.getByRole('button', { name: 'Create and deploy' }).click();
		await expect(page.locator('#f-name')).toHaveAttribute('aria-invalid', 'true');
		await page.locator('#f-name').pressSequentially('x');
		await expect(page.locator('#f-name')).not.toHaveAttribute('aria-invalid', 'true');
		await expect(page.locator('#f-name-error')).toHaveCount(0);
		await expect(page.locator('#f-name')).toHaveAttribute('autocomplete', 'off');
	});

	test('the health radio says it was chosen for you until you pick one', async ({ page, server, admin }) => {
		await signIn(page, server, admin);
		await page.goto(`${server.baseURL}/services/new`);
		await expect(page.locator('#health-hint')).toContainText('Chosen for you');
		await page.getByRole('radio', { name: /^HTTP/ }).check();
		await expect(page.locator('#health-hint')).not.toContainText('Chosen for you');
		await expect(page.getByRole('radio', { name: /Blue-green/ })).toHaveAccessibleDescription(/beside the old one/);
	});

	test('project headings are named "Project <name>" and the sections are not landmarks', async ({ page, server, admin }) => {
		seedThreeServices(server);
		await signIn(page, server, admin);
		await page.goto(`${server.baseURL}/services`);
		await expect(page.getByRole('heading', { name: 'Project shop', level: 2 })).toBeVisible();
		await expect(page.getByRole('region')).toHaveCount(0);
	});
});
