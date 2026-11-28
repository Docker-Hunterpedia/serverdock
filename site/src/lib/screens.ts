import { existsSync, readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import screens from '../data/screens.json';

/** The last sentence of every screenshot caption. Gate 10 checks the built pages for the same text. */
export const DEMO_SENTENCE = 'Demo data: the states are staged with seeded rows and mocked API responses.';

export interface ScreenFile { name: string; width: number; height: number; bytes?: number }
export interface ScreenData {
	id: string;
	alt: string;
	shows: string;
	capturedAt: string;
	viewport: 'desktop' | 'phone';
	dark: ScreenFile[];
	light: ScreenFile[];
	/** URL prefix of the files, with a trailing slash. */
	base: string;
}

interface Manifest {
	captured_at: string;
	entries: { id: string; viewport: 'desktop' | 'phone'; theme: 'dark' | 'light'; files: ScreenFile[] }[];
}

/**
 * One screenshot, from the committed manifest and src/data/screens.json. The build reads files only; it
 * never builds Go or the web UI. Fails the build when the id has no committed images yet.
 */
export function getScreen(id: string): ScreenData {
	const path = resolve(process.cwd(), 'public/screens/manifest.json');
	if (!existsSync(path)) throw new Error(`no screenshots are committed yet (public/screens/manifest.json): cannot show "${id}"`);
	const manifest = JSON.parse(readFileSync(path, 'utf8')) as Manifest;
	const text = (screens as Record<string, { alt: string; shows: string }>)[id];
	if (!text) throw new Error(`src/data/screens.json has no entry for "${id}"`);
	const files = (theme: 'dark' | 'light') => manifest.entries.find((e) => e.id === id && e.theme === theme)?.files.slice().sort((a, b) => a.width - b.width);
	const dark = files('dark');
	const light = files('light');
	const viewport = manifest.entries.find((e) => e.id === id)?.viewport;
	if (!dark || !light || !viewport) throw new Error(`the manifest has no images for "${id}" in both themes`);
	return { id, alt: text.alt, shows: text.shows, capturedAt: manifest.captured_at, viewport, dark, light, base: '/screens/' };
}
