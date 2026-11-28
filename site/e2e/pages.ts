import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

/** Every page in the test build's sitemap, plus the 404 page (served for an unknown path) and /styleguide/. */
export function sitePaths(): string[] {
	const xml = readFileSync(resolve(process.cwd(), 'dist-test/sitemap.xml'), 'utf8');
	const paths = [...xml.matchAll(/<loc>https:\/\/serverdock\.oday-bakkour\.com([^<]*)<\/loc>/g)].map((m) => m[1]);
	return [...new Set([...paths, '/styleguide/', '/this-page-does-not-exist/'])];
}
