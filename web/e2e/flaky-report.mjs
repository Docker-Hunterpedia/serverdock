// Reads the Playwright JSON report, prints how many tests were flaky (failed, then
// passed on the retry) and which, writes the same to the GitHub step summary, and
// exits 1 when more than FLAKY_MAX (default 3) were flaky or any test failed.
import { appendFileSync, existsSync, readFileSync } from 'node:fs';

const file = new URL('./.results/report.json', import.meta.url).pathname;
if (!existsSync(file)) {
	console.error('flaky-report: no report.json (did the tests run?)');
	process.exit(1);
}
const report = JSON.parse(readFileSync(file, 'utf8'));
const max = Number(process.env.FLAKY_MAX ?? 3);
const flaky = [];
const failed = [];
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

const lines = [`Playwright: ${report.stats.expected} passed, ${report.stats.unexpected} failed, ${report.stats.flaky} flaky, ${report.stats.skipped} skipped (flaky limit ${max}).`];
for (const f of flaky) lines.push(`- flaky: ${f}`);
for (const f of failed) lines.push(`- FAILED: ${f}`);
console.log(lines.join('\n'));
if (process.env.GITHUB_STEP_SUMMARY) appendFileSync(process.env.GITHUB_STEP_SUMMARY, lines.join('\n') + '\n');
if (failed.length > 0 || flaky.length > max) process.exit(1);
