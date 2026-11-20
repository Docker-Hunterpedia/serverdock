// The JS budget: the gzip size of everything under build/_app/immutable must stay
// within 250 KB (plan 0003). Prints the first-load JS (the files index.html
// preloads) and the total JS. Exits 1 over budget.
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { gzipSync, constants } from 'node:zlib';
import { join, relative } from 'node:path';

const BUDGET = 250 * 1000; // 250 KB of gzip, 1 KB = 1000 bytes like the bench
const root = new URL('../build', import.meta.url).pathname;

function walk(dir) {
	return readdirSync(dir).flatMap((name) => {
		const p = join(dir, name);
		return statSync(p).isDirectory() ? walk(p) : [p];
	});
}
const gz = (file) => gzipSync(readFileSync(file), { level: constants.Z_BEST_COMPRESSION }).length;

const js = walk(join(root, '_app', 'immutable')).filter((f) => f.endsWith('.js'));
const total = js.reduce((sum, f) => sum + gz(f), 0);

const index = readFileSync(join(root, 'index.html'), 'utf8');
const preloaded = new Set([...index.matchAll(/href="\/(_app\/immutable\/[^"]+\.js)"/g)].map((m) => m[1]));
const firstLoad = js.filter((f) => preloaded.has(relative(root, f))).reduce((sum, f) => sum + gz(f), 0);

const kb = (n) => (n / 1000).toFixed(1) + ' KB';
console.log(`js size: ${js.length} files, first load ${kb(firstLoad)} gzip, total ${kb(total)} gzip (budget ${kb(BUDGET)})`);
if (total > BUDGET) {
	console.error(`js size: over the budget by ${kb(total - BUDGET)}`);
	process.exit(1);
}
