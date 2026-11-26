import { countApiRequests, expect, expectNoAxeViolations, signIn, test } from './fixtures';
import { seedService, seedThreeServices } from './seed';
import { DOCKER_SOCKET, TEST_APP_IMAGE, startServer } from './server';

// Plan 0004 step 12: Services list, New service, Overview. The implementer's smoke
// tests; the tester extends them to every listed screen state in all 8 projects.
test.use({ storageState: { cookies: [], origins: [] } });

test('Overview and Services are empty on a new server', async ({ page, server, admin }) => {
	await signIn(page, server, admin);
	await page.goto(`${server.baseURL}/`);
	await expect(page.getByRole('heading', { name: 'No services yet' })).toBeVisible();
	await expect(page.getByRole('link', { name: 'Deploy an image' })).toBeVisible();
	await expectNoAxeViolations(page, 'Overview, empty');
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
	expect(reqs.seen.filter((p) => p.includes('/actions/services.') || p.includes('/actions/system.exposure'))).toEqual([]);
});

test('New service validates, keeps typed input and deploys the test app for real', async ({ page, allow5xx }) => {
	allow5xx(); // a fresh server with Docker may log a 503 on the first reads while it settles
	const server = await startServer({ dockerSocket: DOCKER_SOCKET, allowLoopbackRegistry: true });
	try {
		const admin = await server.firstRun();
		await signIn(page, server, admin);
		await page.goto(`${server.baseURL}/services/new`);
		await expect(page.getByRole('heading', { name: 'Deploy a Docker image' })).toBeVisible();
		await page.getByLabel('Name').fill('Bad Name');
		await page.getByRole('button', { name: 'Create and deploy' }).click();
		await expect(page.getByRole('alert')).toContainText('fields need fixing');
		await expect(page.getByLabel('Name')).toHaveValue('Bad Name');
		await page.getByLabel('Name').fill('web');
		await page.getByLabel('Image').fill(TEST_APP_IMAGE);
		await page.getByLabel('Port your app listens on (optional)').fill('8080');
		await page.getByRole('button', { name: 'Create and deploy' }).click();
		await expect(page).toHaveURL(/\/services\/default\/web\/deploys\/1$/);
		await expect(page.getByRole('heading', { name: 'Deploy #1' })).toBeVisible();
		await expect.poll(() => server.query<{ status: string }>(`SELECT status FROM deploys WHERE number = 1`)[0]?.status, { timeout: 120_000 }).toBe('succeeded');
		await page.goto(`${server.baseURL}/services`);
		await expect(page.locator('[data-row="default/web"]:visible')).toContainText('Running');
	} finally {
		await server.stop();
	}
});
