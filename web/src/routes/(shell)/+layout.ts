import { error, redirect } from '@sveltejs/kit';
import { ApiError, getSession, signedOutReason } from '../../lib/api';

// Every shell page needs a session: no session means the Signed-out screen.
export async function load() {
	try {
		return { session: await getSession() };
	} catch (e) {
		if (e instanceof ApiError) {
			if (e.signedOut) redirect(307, `/signed-out?reason=${signedOutReason(e.code)}`);
			error(e.status === 0 ? 503 : e.status, e.message);
		}
		throw e;
	}
}
