// Reads the Playwright JSON report, prints how many tests were flaky (failed, then
// passed on the retry) and which, writes the same to the GitHub step summary, and
// exits 1 when more than FLAKY_MAX (default 3) were flaky or any test failed.
import { appendFileSync, existsSync, readdirSync, readFileSync } from 'node:fs';

// Every run writes its own report (outside outputDir, which each run empties) (report-ui.json, report-docker.json, or report-all.json).
const dir = new URL('./.reports/', import.meta.url).pathname;
const files = existsSync(dir) ? readdirSync(dir).filter((f) => /^report(-.+)?\.json$/.test(f)) : [];
if (files.length === 0) {
	console.error('flaky-report: no report json (did the tests run?)');
	process.exit(1);
}
const max = Number(process.env.FLAKY_MAX ?? 3);
const flaky = [];
const failed = [];
const stats = { expected: 0, unexpected: 0, flaky: 0, skipped: 0 };
for (const f of files) {
	const report = JSON.parse(readFileSync(dir + f, 'utf8'));
	for (const k of Object.keys(stats)) stats[k] += report.stats[k] ?? 0;
	(function walk(suite, trail) {
		for (const spec of suite.specs ?? []) {
			for (const t of spec.tests ?? []) {
				const name = `[${t.projectName}] ${[...trail, spec.title].join(' > ')}`;
				if (t.status === 'flaky') flaky.push(name);
				if (t.status === 'unexpected') failed.push(name);
			}
		}
		for (const s of suite.suites ?? []) walk(s, s.title && !s.file ? [...trail, s.title] : trail);
	})({ suites: report.suites }, []);
}

const lines = [`Playwright (${files.join(', ')}): ${stats.expected} passed, ${stats.unexpected} failed, ${stats.flaky} flaky, ${stats.skipped} skipped (flaky limit ${max}).`];
for (const f of flaky) lines.push(`- flaky: ${f}`);
for (const f of failed) lines.push(`- FAILED: ${f}`);
console.log(lines.join('\n'));
if (process.env.GITHUB_STEP_SUMMARY) appendFileSync(process.env.GITHUB_STEP_SUMMARY, lines.join('\n') + '\n');
if (failed.length > 0 || flaky.length > max) process.exit(1);
