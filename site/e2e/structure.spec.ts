import { expect, test } from './fixtures';
import { sitePaths } from './pages';

const TARGETS = [
	'.site-header a', '.site-header summary', '.pill', '.chip', '.site-footer a', '.docs-nav a', '.docs-nav summary',
	'.toc a', '.evidence', 'a.edit', 'a.source', 'summary',
].join(', ');

for (const path of sitePaths()) {
	test.describe(path, () => {
		test('skip link is the first tab stop and moves focus to main', async ({ page, browserName }) => {
			test.skip(browserName === 'webkit', 'WebKit does not Tab to links by default (Safari needs Option+Tab)');
			await page.goto(path);
			await page.keyboard.press('Tab');
			const first = await page.evaluate(() => (document.activeElement as HTMLElement).className);
			expect(first).toBe('skip-link');
			await expect(page.locator('.skip-link')).toBeInViewport();
			await page.keyboard.press('Enter');
			expect(await page.evaluate(() => document.activeElement?.id)).toBe('main');
		});

		test('focus is visible on every link and summary', async ({ page, browserName }) => {
			test.skip(browserName === 'webkit', 'WebKit does not Tab to links by default');
			await page.goto(path);
			const n = await page.locator('a:visible, summary:visible').count();
			for (let i = 0; i < n + 1; i++) {
				await page.keyboard.press('Tab');
				const ring = await page.evaluate(() => {
					const el = document.activeElement as HTMLElement;
					if (!el || el === document.body) return null;
					const s = getComputedStyle(el);
					return { tag: el.tagName, text: el.textContent?.trim().slice(0, 30), style: s.outlineStyle, width: parseFloat(s.outlineWidth), offset: parseFloat(s.outlineOffset) };
				});
				if (ring) expect(ring.style !== 'none' && ring.width >= 2, JSON.stringify(ring)).toBe(true);
			}
		});

		test('one h1 and no skipped heading levels', async ({ page }) => {
			await page.goto(path);
			const levels = await page.$$eval('h1,h2,h3,h4,h5,h6', (hs) => hs.map((h) => Number(h.tagName[1])));
			expect(levels.filter((l) => l === 1)).toHaveLength(1);
			expect(levels[0]).toBe(1);
			for (let i = 1; i < levels.length; i++) expect(levels[i] - levels[i - 1], `heading order ${levels.join(',')}`).toBeLessThanOrEqual(1);
		});

		test('no horizontal scroll outside table regions and code blocks, and the header is not sticky', async ({ page }) => {
			await page.goto(path);
			const r = await page.evaluate(() => ({
				over: document.documentElement.scrollWidth - document.documentElement.clientWidth,
				pos: getComputedStyle(document.querySelector('header')!).position,
			}));
			expect(r.over).toBeLessThanOrEqual(0);
			expect(['static', 'relative']).toContain(r.pos);
		});

		test('one main nav is in the accessibility tree and links with the same name go to the same place', async ({ page }) => {
			await page.goto(path);
			const navs = await page.$$eval('nav', (ns) => ns.filter((n) => n.getClientRects().length > 0).map((n) => n.getAttribute('aria-label')));
			expect(new Set(navs).size, `visible navs ${navs}`).toBe(navs.length);
			const links = await page.$$eval('a:not(.skip-link)', (as) => as.filter((a) => a.getClientRects().length > 0).map((a) => [a.textContent!.replace(/\s+/g, ' ').trim(), (a as HTMLAnchorElement).getAttribute('href')]));
			const byName = new Map<string, Set<string>>();
			for (const [name, href] of links) byName.set(name!, (byName.get(name!) ?? new Set()).add(href!));
			for (const [name, hrefs] of byName) expect(hrefs.size, `link name "${name}" is used for ${[...hrefs]}`).toBe(1);
		});

		test('the narrow menu opens with Enter and Space', async ({ page, viewport }) => {
			test.skip((viewport?.width ?? 1280) >= 768, 'the Menu is shown below 48rem only');
			await page.goto(path);
			const summary = page.locator('.nav-narrow summary');
			await summary.focus();
			await page.keyboard.press('Enter');
			await expect(page.locator('.nav-narrow')).toHaveAttribute('open', '');
			await page.keyboard.press('Space');
			await expect(page.locator('.nav-narrow')).not.toHaveAttribute('open', '');
			await page.keyboard.press('Space');
			await expect(page.locator('.nav-narrow')).toHaveAttribute('open', '');
		});

		test('targets are at least 44x44 px, and in-text links at least 24 px', async ({ page, viewport }) => {
			test.skip((viewport?.width ?? 1280) > 700, 'checked at phone, 320 px and 400% zoom widths');
			await page.goto(path);
			// open every <details> so the narrow-nav links are laid out
			await page.$$eval('details', (ds) => ds.forEach((d) => d.setAttribute('open', '')));
			const small = await page.$$eval(TARGETS, (els) =>
				els.filter((e) => e.getClientRects().length > 0).map((e) => {
					const r = e.getBoundingClientRect();
					return { text: e.textContent!.trim().slice(0, 30), w: Math.round(r.width), h: Math.round(r.height) };
				}).filter((r) => r.w < 44 || r.h < 44));
			expect(small).toEqual([]);
			const inText = await page.$$eval('main p a:not([class]), main li a:not([class])', (els) =>
				els.filter((e) => e.getClientRects().length > 0).map((e) => ({ text: e.textContent!.trim().slice(0, 30), h: Math.round(e.getBoundingClientRect().height) })).filter((r) => r.h < 24));
			expect(inText).toEqual([]);
		});

		test('text spacing (WCAG 1.4.12) clips and overlaps nothing', async ({ browser, baseURL, browserName }, info) => {
			test.skip(info.project.name !== 'desktop' && info.project.name !== 'phone-320' && browserName !== 'webkit', 'run at desktop, 320 px and WebKit');
			const ctx = await browser.newContext({ bypassCSP: true, viewport: info.project.use.viewport ?? { width: 1280, height: 800 }, baseURL });
			try {
				const page = await ctx.newPage();
				await page.goto(path);
				await page.addStyleTag({ content: '*{line-height:1.5 !important;letter-spacing:.12em !important;word-spacing:.16em !important} p{margin-bottom:2em !important}' });
				await page.$$eval('details', (ds) => ds.forEach((d) => d.setAttribute('open', '')));
				const clipped = await page.$$eval('body *', (els) =>
					els.filter((e) => {
						if (e.closest('.sr-only') || e.classList.contains('sr-only') || e.closest('svg')) return false;
						const s = getComputedStyle(e);
						const clips = ['hidden', 'clip'].includes(s.overflowX) || ['hidden', 'clip'].includes(s.overflowY);
						return clips && (e.scrollHeight > e.clientHeight + 1 || e.scrollWidth > e.clientWidth + 1);
					}).map((e) => e.tagName + '.' + e.className));
				expect(clipped).toEqual([]);
				const over = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
				expect(over).toBeLessThanOrEqual(0);
			} finally {
				await ctx.close();
			}
		});
	});
}
