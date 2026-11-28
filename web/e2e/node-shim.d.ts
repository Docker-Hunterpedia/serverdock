// A loose stand-in for @types/node, only for `tsc -p tsconfig.e2e.json` (plan 0014, Revision 3).
// The Node modules the e2e helpers use are typed as `any`; the point of that check is the shape of the
// mocked API answers, which are typed against src/lib/api-types.ts. If this grows, add @types/node as a
// pinned dev dependency (with footprint sign-off) instead.
declare module 'node:child_process' {
	export type ChildProcess = any;
	export const spawn: any;
	export const spawnSync: any;
}
declare module 'node:crypto';
declare module 'node:fs';
declare module 'node:net';
declare module 'node:os';
declare module 'node:path';
declare module 'node:sqlite';
declare module 'node:url';
declare module 'node:zlib';
declare const process: { env: Record<string, string | undefined>; platform: string; pid: number; cwd(): string; on(event: string, fn: () => void): void };
declare const Buffer: any;
// eslint-disable-next-line @typescript-eslint/no-explicit-any
type Buffer = any;
declare const console: { log(...a: unknown[]): void; error(...a: unknown[]): void };
