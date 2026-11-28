import { expect, expectNoAxeViolations, letTheAccountAge, signIn, test } from './fixtures';
import { Socket, createServer, type Server } from 'node:net';
import { DOCKER_SOCKET, TEST_APP_IMAGE, startServer, uniqueProject } from './server';

// Plan 0004 step 14, real Docker: the page follows a real deploy to its end by itself,
// and Redeploy from the live deploy's page leads to the next deploy, whose success
// makes the first deploy's log available. Runs in the docker-* projects, one worker.
test.use({ storageState: { cookies: [], origins: [] } });

test('a real deploy is followed live to Live, then Redeploy opens the next deploy and keeps the first log', async ({ page, allow5xx }) => {
	test.setTimeout(300_000);
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
		await page.getByLabel('Image').fill(TEST_APP_IMAGE);
		await page.getByLabel('Port your app listens on (optional)').fill('8080');
		await page.getByRole('button', { name: 'Create and deploy' }).click();
		await expect(page).toHaveURL(new RegExp(`/services/${project}/web/deploys/1$`));
		// No reload, no Refresh: the page ends at Live by polling.
		await expect(page.getByTestId('deploy-status')).toContainText('Live', { timeout: 180_000 });
		const stages = page.getByTestId('stages').locator('li');
		await expect(stages.first()).toContainText('Done');
		// A first deploy has no previous version: its drain and stop stages are skipped, the rest are done.
		for (const li of await stages.all()) await expect(li).toContainText(/Done|Skipped/);
		await expect(page.getByTestId('stage-health')).toContainText('Done');
		await expect(page.getByTestId('stage-switch')).toContainText('Done');
		await expect(page.getByTestId('app-safety')).toContainText('the new version is live');
		await expect(page.getByRole('status')).toContainText('succeeded');
		await expect(page.getByTestId('log-message')).toContainText('kept when a newer deploy replaces it');
		await expectNoAxeViolations(page, 'Deploy detail, a real live deploy');

		await page.getByTestId('redeploy').click();
		await page.getByRole('dialog').getByRole('button', { name: 'Redeploy' }).click();
		await expect(page).toHaveURL(new RegExp(`/services/${project}/web/deploys/2$`));
		await expect(page.getByTestId('deploy-status')).toContainText('Live', { timeout: 180_000 });
		await page.goto(`${server.baseURL}/services/${project}/web/deploys/1`);
		await expect(page.getByTestId('deploy-status')).toContainText('Replaced');
		await expect(page.getByTestId('log-tail')).toBeVisible();
		await expect(page.getByTestId('rollback')).toBeVisible();
		await expectNoAxeViolations(page, 'Deploy detail, a replaced deploy with its log');
	} finally {
		await server.stop();
	}
});

/**
 * A TCP proxy in front of the local test registry that hands every answer to the client
 * `delayMs` late, so a pull of the (tiny) test image takes minutes: a slow registry.
 */
async function slowRegistry(delayMs: number) {
	const upstream = Number(new URL(`http://${TEST_APP_IMAGE.split('/')[0]}`).port);
	const sockets = new Set<Socket>();
	const proxy: Server = createServer((client) => {
		const up = new Socket();
		sockets.add(client).add(up);
		up.connect(upstream, '127.0.0.1');
		let tail = Promise.resolve();
		client.on('data', (d) => up.write(d));
		up.on('data', (d) => {
			tail = tail.then(() => new Promise<void>((r) => setTimeout(() => (client.destroyed || client.write(d), r()), delayMs)));
		});
		const end = () => {
			client.destroy();
			up.destroy();
		};
		client.on('close', end).on('error', end);
		// What the registry sent before it closed still arrives, late, before the client sees the end.
		up.on('close', () => void tail.then(() => client.end()));
		up.on('error', end);
	});
	await new Promise<void>((r) => proxy.listen(0, '127.0.0.1', r));
	const port = (proxy.address() as { port: number }).port;
	return {
		image: `127.0.0.1:${port}/${TEST_APP_IMAGE.split('/').slice(1).join('/')}`,
		stop: async () => {
			sockets.forEach((x) => x.destroy());
			await new Promise((r) => proxy.close(r));
		}
	};
}

test('Cancel works while a real image is pulling from a slow registry, and the page ends at Cancelled by itself', async ({ page, allow5xx }) => {
	test.setTimeout(240_000);
	allow5xx();
	const registry = await slowRegistry(4000);
	const server = await startServer({ dockerSocket: DOCKER_SOCKET, allowLoopbackRegistry: true });
	try {
		const admin = await server.firstRun();
		await letTheAccountAge();
		await signIn(page, server, admin);
		const project = uniqueProject();
		await page.goto(`${server.baseURL}/services/new`);
		await page.getByLabel('Project').fill(project);
		await page.getByLabel('Name').fill('slow');
		await page.getByLabel('Image').fill(registry.image);
		await page.getByLabel('Port your app listens on (optional)').fill('8080');
		await page.getByRole('button', { name: 'Create and deploy' }).click();
		await expect(page).toHaveURL(new RegExp(`/services/${project}/slow/deploys/1$`));
		// Pulling for real: the stage is running, the bar is there, and Cancel is offered.
		await expect(page.getByTestId('stage-pull')).toHaveAttribute('data-state', 'running', { timeout: 60_000 });
		await expect(page.getByTestId('deploy-status')).toContainText('Running');
		await expect(page.getByTestId('pull-progress')).toBeVisible();
		const cancel = page.getByTestId('cancel-deploy');
		await expect(cancel).not.toHaveAttribute('aria-disabled', 'true');
		await page.waitForTimeout(2500); // the page has polled at least once while pulling
		await expect(page.getByTestId('stage-pull')).toHaveAttribute('data-state', 'running');
		await cancel.click();
		// No reload: the page itself arrives at Cancelled (it may pass through "Cancel requested").
		await expect(page.getByTestId('deploy-status')).toContainText('Cancelled', { timeout: 90_000 });
		await expect(page.getByTestId('deploy-reason')).toContainText('It was cancelled');
		await expect(page.getByRole('status')).toHaveText('Deploy #1 was cancelled.');
		await expect(page.getByTestId('cancel-deploy')).toHaveCount(0);
		await expect(page.getByTestId('pull-progress')).toHaveCount(0);
		await expect(page.getByRole('alert')).toHaveText('');
		await expectNoAxeViolations(page, 'Deploy detail, a real cancelled pull');
		// The database agrees, and nothing is left running for the service.
		expect(server.query<{ status: string }>(`SELECT d.status FROM deploys d JOIN services s ON s.id = d.service_id WHERE s.name = 'slow'`)[0].status).toBe('cancelled');
	} finally {
		await server.stop();
		await registry.stop();
	}
});
