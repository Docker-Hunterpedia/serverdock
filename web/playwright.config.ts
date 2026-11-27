import { defineConfig, devices } from '@playwright/test';

// The suite runs against the real binary (bin/serverdock, built with -tags spa) on
// high ports with a temp data directory: e2e/global-setup.ts starts it, does the
// first run through the API and saves the signed-in cookies. Override the ports
// with E2E_HTTPS_PORT / E2E_HTTP_PORT and the binary with SERVERDOCK_BIN.
export const HTTPS_PORT = Number(process.env.E2E_HTTPS_PORT ?? 18443);
export const BASE_URL = `https://127.0.0.1:${HTTPS_PORT}`;
export const STORAGE = 'e2e/.auth.json';

// The tests that deploy for real (*.docker.spec.ts) run apart from the UI ones: a Docker
// network or container appearing changes the host's network interfaces, and Chromium then
// aborts the in-flight requests of every other test (net::ERR_NETWORK_CHANGED). CI runs
// them as their own step, one worker: E2E_SET=ui, then E2E_SET=docker --workers=1.
// Unset, one run does both: the docker projects wait for all the UI projects.
const SET = process.env.E2E_SET ?? 'all';
const DOCKER_SPECS = /.*\.docker\.spec\.ts/;

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
	reporter: process.env.CI ? [['list'], ['github'], ['json', { outputFile: `e2e/.reports/report-${SET}.json` }]] : 'list',
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
		.map((p) => ({ ...p, testIgnore: DOCKER_SPECS }))
		.filter(() => SET !== 'docker')
		.concat(
			[
				// Real flows on a desktop and on a phone (WebKit is not needed: the flows are the same).
				{ name: 'docker-desktop', use: { ...devices['Desktop Chrome'], viewport: { width: 1280, height: 800 } } },
				{ name: 'docker-phone', use: { ...devices['Desktop Chrome'], viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true } }
			]
				.filter(() => SET !== 'ui')
				.map((p, i, all) => ({
					...p,
					testMatch: DOCKER_SPECS,
					fullyParallel: false,
					// Serial even in a local all-in-one run (Docker networks and containers appearing change the host's interfaces).
					workers: 1,
					// One after the other, and (unset SET) after every UI project.
					dependencies: SET === 'all' ? ['desktop', 'phone-390', 'phone-320', 'zoom-200', 'dark', 'reduced-motion', 'forced-colors', 'webkit'].concat(i > 0 ? [all[i - 1].name] : []) : i > 0 ? [all[i - 1].name] : []
				}))
		)
});
