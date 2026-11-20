import AxeBuilder from '@axe-core/playwright';
import { expect, test as base, type Page } from '@playwright/test';

export const AXE_TAGS = ['wcag2a', 'wcag2aa', 'wcag21aa', 'wcag22aa', 'best-practice'];

/** Zero axe violations under the A5 tags, with a readable failure message. */
export async function expectNoAxeViolations(page: Page, what: string) {
	const { violations } = await new AxeBuilder({ page }).withTags(AXE_TAGS).analyze();
	const summary = violations.map((v) => `${v.id} (${v.impact}): ${v.nodes.map((n) => n.target.join(' ')).join(', ')}`);
	expect(summary, `axe violations on ${what}`).toEqual([]);
}

// Every test also fails on a CSP violation, a page error or a console error.
export const test = base.extend<{ problems: string[] }>({
	problems: [
		async ({ page }, use) => {
			const problems: string[] = [];
			await page.addInitScript(() => {
				addEventListener('securitypolicyviolation', (e) => {
					console.error('CSP violation: ' + e.violatedDirective + ' ' + e.blockedURI);
				});
			});
			page.on('pageerror', (e) => problems.push('pageerror: ' + e.message));
			page.on('console', (m) => {
				if (m.type() === 'error') problems.push('console: ' + m.text());
			});
			await use(problems);
			expect(problems, 'CSP violations, page errors or console errors').toEqual([]);
		},
		{ auto: true }
	]
});
export { expect };
