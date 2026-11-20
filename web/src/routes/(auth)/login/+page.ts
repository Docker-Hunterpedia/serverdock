import { redirect } from '@sveltejs/kit';
import { getSession, setupStatus } from '../../../lib/api';

// A fresh install has nobody to sign in: go to setup. Already signed in: go home.
export async function load() {
	let setupRequired = false;
	try {
		setupRequired = (await setupStatus()).setup_required;
	} catch {
		/* the page still works; the sign-in call reports the problem */
	}
	if (setupRequired) redirect(307, '/setup');
	let signedIn = false;
	try {
		await getSession();
		signedIn = true;
	} catch {
		/* not signed in: show the form */
	}
	if (signedIn) redirect(307, '/');
	return {};
}
