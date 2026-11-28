import { expect, test } from './fixtures';

test.describe('styleguide components', () => {
	test('meter: the SVG is decorative and the fill width is value / max', async ({ page }) => {
		await page.goto('/styleguide/');
		await expect(page.locator('.meter svg')).toHaveAttribute('aria-hidden', 'true');
		await expect(page.locator('.meter .fill')).toHaveAttribute('width', '5.3');
		await expect(page.locator('.meter-text')).toHaveText('54 MB of 1 GB');
		expect(await page.locator('meter, progress, [role=meter], [role=progressbar]').count()).toBe(0);
	});

	test('deploy stages: an ol with hidden state words, no role=img, no aria-live', async ({ page }) => {
		await page.goto('/styleguide/');
		await expect(page.locator('figure.stages-figure > ol')).toHaveCount(1);
		await expect(page.locator('.stages li').first().locator('.sr-only')).toHaveText(/done/);
		expect(await page.locator('.stages-figure [role=img], .stages-figure [aria-live]').count()).toBe(0);
	});

	test('pill: name starts with its visible text and has no aria-label', async ({ page }) => {
		await page.goto('/styleguide/');
		const pill = page.locator('.pill');
		expect(await pill.getAttribute('aria-label')).toBeNull();
		await expect(pill).toHaveText(/^Pre-release · Phase 1\. See project status$/);
		await expect(page.getByRole('link', { name: /^Pre-release · Phase 1/ })).toHaveCount(1);
	});

	test('table region: labelled, focusable, native table display, cue only below 37.5rem', async ({ page, viewport }) => {
		await page.goto('/styleguide/');
		const region = page.locator('.table-region');
		await expect(region).toHaveAttribute('role', 'region');
		await expect(region).toHaveAttribute('tabindex', '0');
		const label = await region.getAttribute('aria-labelledby');
		await expect(page.locator(`#${label}`)).toHaveText('Example figures');
		expect(await page.locator('.table-region table').evaluate((t) => getComputedStyle(t).display)).toBe('table');
		const narrow = (viewport?.width ?? 1280) < 600;
		await expect(page.locator('.scroll-cue')).toBeVisible({ visible: narrow } as never).catch(() => undefined);
		expect(await page.locator('.scroll-cue').isVisible()).toBe(narrow);
	});

	test('table region: the sticky first column is at most 40% of the region at 320 px', async ({ page, viewport }) => {
		test.skip((viewport?.width ?? 1280) > 340, '320 px only');
		await page.goto('/styleguide/');
		const m = await page.evaluate(() => {
			const region = document.querySelector('.table-region')!.getBoundingClientRect();
			const th = document.querySelector('.table-region tbody th')!;
			return { region: region.width, th: th.getBoundingClientRect().width, pos: getComputedStyle(th).position };
		});
		expect(m.pos).toBe('sticky');
		expect(m.th).toBeLessThanOrEqual(m.region * 0.4 + 1);
	});

	test('the docs sidebar comes before main in the DOM', async ({ page }) => {
		await page.goto('/styleguide/');
		expect(await page.evaluate(() => document.querySelector('.docs-nav')!.compareDocumentPosition(document.querySelector('main')!) & Node.DOCUMENT_POSITION_FOLLOWING)).toBeTruthy();
	});

	test('forced colours: the meter and the stage dots use system colours and borders stay', async ({ page }, info) => {
		test.skip(info.project.name !== 'forced-colors', 'forced-colors project only');
		await page.goto('/styleguide/');
		const m = await page.evaluate(() => {
			const cs = (sel: string) => getComputedStyle(document.querySelector(sel)!);
			return { fill: cs('.meter .fill').fill, track: cs('.meter .track').fill, trackStroke: cs('.meter .track').stroke, pill: cs('.pill').borderTopStyle, card: cs('.card').borderTopStyle, region: cs('.table-region').borderTopStyle };
		});
		expect(m.fill).not.toBe(m.track);
		expect(m.trackStroke).not.toBe('none');
		for (const b of [m.pill, m.card, m.region]) expect(b).not.toBe('none');
	});

	test('light and dark: the page background follows the scheme', async ({ page }, info) => {
		test.skip(!['dark', 'light'].includes(info.project.name), 'dark and light projects only');
		await page.goto('/styleguide/');
		const bg = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
		expect(bg).toBe(info.project.name === 'dark' ? 'rgb(11, 15, 20)' : 'rgb(255, 255, 255)');
	});

	test('reduced motion: nothing animates', async ({ page }, info) => {
		test.skip(info.project.name !== 'reduced-motion', 'reduced-motion project only');
		await page.goto('/styleguide/');
		const anim = await page.evaluate(() => [...document.querySelectorAll('*')].filter((e) => getComputedStyle(e).animationName !== 'none').length);
		expect(anim).toBe(0);
		expect(await page.evaluate(() => getComputedStyle(document.documentElement).scrollBehavior)).toBe('auto');
	});
});
