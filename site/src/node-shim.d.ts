// The two Node APIs src/pages/og.png.ts uses, so @types/node is not a dependency.
declare module 'node:fs' {
	export function lstatSync(path: string): { isSymbolicLink(): boolean; isFile(): boolean };
	export function readFileSync(path: string): Uint8Array<ArrayBuffer>;
	export function readFileSync(path: string, encoding: 'utf8'): string;
	export function existsSync(path: string): boolean;
}
declare module 'node:path' {
	export function resolve(...paths: string[]): string;
}
declare const process: { cwd(): string; env: Record<string, string | undefined> };
