import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

// docs/assets/og.png is the single copy; it is read at build time, never committed twice.
// The build always runs from site/ (npm scripts), so the path is relative to the working directory.
export function GET() {
	const png = readFileSync(resolve(process.cwd(), '../docs/assets/og.png'));
	return new Response(png, { headers: { 'Content-Type': 'image/png' } });
}
