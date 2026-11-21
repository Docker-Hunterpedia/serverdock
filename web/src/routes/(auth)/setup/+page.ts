import { redirect } from '@sveltejs/kit';
import { ApiError, setupStatus, type SetupStatus } from '../../../lib/api';

// First run only: once an admin exists the setup endpoints are gone, so go sign in.
export async function load(): Promise<{ status: SetupStatus | null; loadError: string | null }> {
	try {
		const status = await setupStatus();
		if (!status.setup_required) redirect(307, '/login');
		return { status, loadError: null };
	} catch (e) {
		if (e instanceof ApiError) return { status: null, loadError: e.message };
		throw e;
	}
}
