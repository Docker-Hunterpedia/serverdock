/** An absolute time with its zone, "2026-11-20 14:02 UTC" (never "3 hours ago"). */
export function absolute(iso: string | number | Date | null | undefined): string {
	if (iso === null || iso === undefined || iso === '') return 'never';
	const d = new Date(typeof iso === 'number' ? iso * 1000 : iso);
	if (Number.isNaN(d.getTime()) || d.getUTCFullYear() < 2000) return 'unknown';
	return d.toISOString().slice(0, 16).replace('T', ' ') + ' UTC';
}

export const DAY_MS = 86_400_000;
export const daysUntil = (iso: string, now = Date.now()) => Math.floor((new Date(iso).getTime() - now) / DAY_MS);
