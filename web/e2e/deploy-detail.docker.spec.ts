import { expect, expectNoAxeViolations, letTheAccountAge, signIn, test } from './fixtures';
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
