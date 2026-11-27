// State that has to survive a route change within one visit, held in memory only
// (never in storage): the user name between the password and code steps, when the
// pending two-factor step ends, and what the Overview should tell you about this
// sign-in (failed attempts, an unwritten audit record).
import type { Notice } from './api';

export const flow = $state({ username: '', pendingUntil: 0 });
export const flash = $state<{ notice?: Notice; audit?: string }>({});

/** True while recovery codes are on screen and not yet marked saved (the nav Sign out asks first). */
export const guard = $state({ unsavedCodes: false });

/**
 * What the Deploy page shows once after "Create and deploy": which service and deploy
 * number was just created, the warnings (shares_panel_address) and, when the proxy did
 * not apply the route yet (applied false), the server's notice. The page takes it and
 * clears it, so it is never shown twice or for another deploy.
 */
export type Created = {
	service?: string;
	number?: number;
	warnings?: { code: string; message: string }[];
	port?: number | null;
	applied?: boolean;
	notice?: string;
};
export const created = $state<Created>({});

/** Takes what was stored for this deploy (if it is the one just created) and clears it. */
export function takeCreated(service: string, number: number): Created | undefined {
	const mine = created.service === service && created.number === number ? { ...created, warnings: [...(created.warnings ?? [])] } : undefined;
	for (const k of Object.keys(created) as (keyof Created)[]) delete created[k];
	return mine;
}
