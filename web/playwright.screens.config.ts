import { defineConfig, devices } from '@playwright/test';

// The screenshot capture for the public website (plan 0014, Revision 3). It has its own config so
// `playwright.config.ts` is not an input of the screenshots, and `npm run e2e` never runs it.
// Run: SCREENS_OUT=<scratch dir> npm run screens   (needs bin/serverdock built with -tags spa).
// It never writes into site/public/screens/: the committed set comes from the CI artifact only.
export default defineConfig({
	testDir: 'e2e',
	testMatch: /screens\.capture\.ts$/,
	outputDir: 'e2e/.results-screens',
	fullyParallel: false,
	workers: 1, // one capture at a time: the same bytes for the same inputs
	retries: 0,
	forbidOnly: !!process.env.CI,
	reporter: 'list',
	timeout: 90_000,
	use: {
		...devices['Desktop Chrome'],
		ignoreHTTPSErrors: true, // the panel's own self-signed certificate
		launchOptions: process.env.E2E_CHROMIUM ? { executablePath: process.env.E2E_CHROMIUM } : {}
	},
	projects: [{ name: 'screens' }]
});
