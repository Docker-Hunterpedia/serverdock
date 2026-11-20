import { type FullConfig } from '@playwright/test';
import { HTTPS_PORT, STORAGE } from '../playwright.config';
import { startServer } from './server';

// One shared, already-set-up server for the read-only shell tests; its admin's
// session cookie is saved for them. Tests that change state start their own server
// (e2e/fixtures.ts), so nothing here is ever consumed.
export default async function globalSetup(_config: FullConfig) {
	const server = await startServer({ https: HTTPS_PORT });
	try {
		await server.firstRun();
		await server.api.storageState({ path: STORAGE });
	} catch (e) {
		await server.stop();
		throw e;
	}
	return () => server.stop();
}
