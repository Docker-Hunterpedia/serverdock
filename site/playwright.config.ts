import { defineConfig, devices } from '@playwright/test';

// Runs against scripts/serve.mjs, which serves the test build (dist-test, with /styleguide/)
// with the real _headers rules, so every test runs under the production CSP.
// Build first: npm run build:test.
const PORT = Number(process.env.SITE_E2E_PORT ?? 4322);
export const BASE_URL = `http://127.0.0.1:${PORT}`;
const desktop = { ...devices['Desktop Chrome'], viewport: { width: 1280, height: 800 } };

export default defineConfig({
	testDir: 'e2e',
	outputDir: 'e2e/.results',
	fullyParallel: true,
	forbidOnly: !!process.env.CI,
	retries: 0, // a retry would hide a flaky accessibility check
	// If the 10-minute job ceiling is approached, workers is the lever (CI uses 2; the job has 2 vCPUs).
	workers: process.env.CI ? 2 : undefined,
	reporter: process.env.CI ? [['list'], ['github']] : 'list',
	use: { baseURL: BASE_URL, trace: 'retain-on-failure' },
	webServer: {
		command: `node scripts/serve.mjs dist-test ${PORT}`,
		env: { SERVE_NO_UPGRADE: '1' }, // WebKit would upgrade plain-http loopback to https; the CSP is otherwise as shipped
		url: `${BASE_URL}/`,
		reuseExistingServer: !process.env.CI,
		timeout: 15_000,
	},
	projects: [
		{ name: 'desktop', use: desktop },
		{ name: 'phone', use: { ...devices['Pixel 7'] } },
		{ name: 'phone-320', use: { ...devices['Pixel 7'], viewport: { width: 320, height: 568 } } },
		// 200% zoom of a 1280 x 800 window is a 640 x 400 CSS-pixel viewport at twice the density.
		{ name: 'zoom-200', use: { ...desktop, viewport: { width: 640, height: 400 }, deviceScaleFactor: 2 } },
		// 400% zoom of a 1280 px window is 320 CSS px at four times the density.
		{ name: 'zoom-400', use: { ...desktop, viewport: { width: 320, height: 200 }, deviceScaleFactor: 4 } },
		{ name: 'dark', use: { ...desktop, colorScheme: 'dark' } },
		{ name: 'light', use: { ...desktop, colorScheme: 'light' } },
		{ name: 'reduced-motion', use: { ...desktop, reducedMotion: 'reduce' } },
		{ name: 'forced-colors', use: { ...desktop, forcedColors: 'active' } },
		{ name: 'contrast-more', use: { ...desktop, contrast: 'more' } },
		{ name: 'webkit', use: { ...devices['Desktop Safari'], viewport: { width: 1280, height: 800 } } },
	],
});
