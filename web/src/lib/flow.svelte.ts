// State that has to survive a route change within one visit, held in memory only
// (never in storage): the user name between the password and code steps, when the
// pending two-factor step ends, and what the Overview should tell you about this
// sign-in (failed attempts, an unwritten audit record).
import type { Notice } from './api';

export const flow = $state({ username: '', pendingUntil: 0 });
export const flash = $state<{ notice?: Notice; audit?: string }>({});
