import AxeBuilder from '@axe-core/playwright';
import { expect, test } from './fixtures';
import { sitePaths } from './pages';

// A colour-contrast "incomplete" result fails unless it is listed here with a reason for the
// manual pass (the launch checklist). Keep this empty unless accessibility signs off.
const INCOMPLETE_CONTRAST_OK: { page: string; reason: string }[] = [];

const TAGS = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa', 'best-practice'];

for (const path of sitePaths()) {
	test(`axe: ${path}`, async ({ page }) => {
		const res = await page.goto(path);
		expect(res?.status()).toBe(path.includes('does-not-exist') ? 404 : 200);
		const r = await new AxeBuilder({ page }).withTags(TAGS).analyze();
		expect(r.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(', ')}`)).toEqual([]);
		const incomplete = r.incomplete.filter((i) => i.id === 'color-contrast' && !INCOMPLETE_CONTRAST_OK.some((o) => o.page === path));
		expect(incomplete.map((v) => v.nodes.map((n) => n.target.join(' ')).join(', ')), 'color-contrast incomplete').toEqual([]);
	});
}
