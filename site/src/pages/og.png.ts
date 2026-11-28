import { lstatSync, readFileSync } from 'node:fs';
import { resolve } from 'node:path';

// docs/assets/og.png is the single copy; it is read at build time, never committed twice.
// The build always runs from site/ (npm scripts), so the path is relative to the working directory.
const MAGIC = [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a];

export function GET() {
	const path = resolve(process.cwd(), '../docs/assets/og.png');
	const st = lstatSync(path);
	if (st.isSymbolicLink() || !st.isFile()) throw new Error('docs/assets/og.png must be a regular file, not a symlink');
	const png = readFileSync(path);
	if (!MAGIC.every((b, i) => png[i] === b)) throw new Error('docs/assets/og.png is not a PNG');
	return new Response(png, { headers: { 'Content-Type': 'image/png' } });
}
