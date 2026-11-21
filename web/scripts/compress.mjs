// Writes <file>.gz (gzip -9) next to every .js and .css file under build/, so the
// Go server can serve the files pre-compressed and compress nothing at runtime.
import { readdirSync, readFileSync, statSync, writeFileSync } from 'node:fs';
import { gzipSync, constants } from 'node:zlib';
import { join } from 'node:path';

const root = new URL('../build', import.meta.url).pathname;

function walk(dir) {
	return readdirSync(dir).flatMap((name) => {
		const p = join(dir, name);
		return statSync(p).isDirectory() ? walk(p) : [p];
	});
}

let n = 0;
for (const file of walk(root)) {
	if (!/\.(js|css)$/.test(file)) continue;
	writeFileSync(file + '.gz', gzipSync(readFileSync(file), { level: constants.Z_BEST_COMPRESSION }));
	n++;
}
console.log(`compress: wrote ${n} .gz files`);
