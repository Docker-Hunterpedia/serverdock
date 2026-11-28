import { expect, expectNoAxeViolations, letTheAccountAge, signIn, test } from './fixtures';
import { seedThreeServices } from './seed';
import { DOCKER_SOCKET, TEST_APP_IMAGE, startServer, uniqueProject } from './server';

// Plan 0004 step 12: the tests that start a server with a real Docker socket. They run
// in the docker-* projects, one worker at a time (see service-detail.docker.spec.ts).
test.use({ storageState: { cookies: [], origins: [] } });

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

test.describe("Services list on a real Docker socket", () => {
	test('a real Docker socket makes a seeded service with no container "Missing"', async ({ page, allow5xx }) => {
		test.setTimeout(90_000); // the first resync after the start takes a while on a loaded machine
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
			}).toPass({ timeout: 60_000 });
			await expect(row).toContainText('◆');
			await expectNoAxeViolations(page, 'Services, a missing container');
		} finally {
			await server.stop();
		}
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
