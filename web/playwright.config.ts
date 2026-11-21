import { defineConfig, devices } from '@playwright/test';

// The suite runs against the real binary (bin/serverdock, built with -tags spa) on
// high ports with a temp data directory: e2e/global-setup.ts starts it, does the
// first run through the API and saves the signed-in cookies. Override the ports
// with E2E_HTTPS_PORT / E2E_HTTP_PORT and the binary with SERVERDOCK_BIN.
export const HTTPS_PORT = Number(process.env.E2E_HTTPS_PORT ?? 18443);
export const BASE_URL = `https://127.0.0.1:${HTTPS_PORT}`;
export const STORAGE = 'e2e/.auth.json';

export default defineConfig({
	testDir: 'e2e',
	outputDir: 'e2e/.results',
	fullyParallel: true,
	forbidOnly: !!process.env.CI,
	// One retry on CI, and the run reports how many tests needed it: e2e/flaky-report.mjs
	// (a CI step) prints the count and fails above a small threshold, so a flaky test
	// is visible and cannot hide behind the retry.
	retries: process.env.CI ? 1 : 0,
	workers: process.env.CI ? 2 : undefined,
	reporter: process.env.CI ? [['list'], ['github'], ['json', { outputFile: 'e2e/.results/report.json' }]] : 'list',
	globalSetup: './e2e/global-setup.ts',
	use: {
		baseURL: BASE_URL,
		ignoreHTTPSErrors: true, // the panel's own self-signed certificate
		storageState: STORAGE,
		trace: 'retain-on-failure',
		// Local escape hatch when the matching browser build is not installed.
		launchOptions: process.env.E2E_CHROMIUM ? { executablePath: process.env.E2E_CHROMIUM } : {}
	},
	// The A5 projects (plan 0003): desktop, phone, 320 px, 200% zoom, dark,
	// reduced motion, forced colours and WebKit.
	projects: [
		{ name: 'desktop', use: { ...devices['Desktop Chrome'], viewport: { width: 1280, height: 800 } } },
		{
			name: 'phone-390',
			use: { ...devices['Desktop Chrome'], viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true }
		},
		{ name: 'phone-320', use: { ...devices['Desktop Chrome'], viewport: { width: 320, height: 568 }, isMobile: true, hasTouch: true } },
		// 200% zoom of a 1280 x 800 window is a 640 x 400 CSS-pixel viewport at twice the density.
		{ name: 'zoom-200', use: { ...devices['Desktop Chrome'], viewport: { width: 640, height: 400 }, deviceScaleFactor: 2 } },
		{ name: 'dark', use: { ...devices['Desktop Chrome'], viewport: { width: 1280, height: 800 }, colorScheme: 'dark' } },
		{ name: 'reduced-motion', use: { ...devices['Desktop Chrome'], viewport: { width: 1280, height: 800 }, reducedMotion: 'reduce' } },
		{ name: 'forced-colors', use: { ...devices['Desktop Chrome'], viewport: { width: 1280, height: 800 }, forcedColors: 'active' } },
		{
			name: 'webkit',
			use: {
				...devices['Desktop Safari'],
				viewport: { width: 1280, height: 800 },
				launchOptions: process.env.E2E_WEBKIT ? { executablePath: process.env.E2E_WEBKIT } : {}
			}
		}
	]
});
