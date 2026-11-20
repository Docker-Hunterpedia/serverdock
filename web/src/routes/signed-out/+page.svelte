<script lang="ts">
	import { page } from '$app/state';

	const reasons: Record<string, string> = {
		signed_out: 'You signed out.',
		idle: 'You were signed out after 12 hours without activity.',
		expired: 'You were signed out because the 7-day session limit was reached.',
		step_up: 'You were signed out after too many wrong confirmation codes.',
		credentials: 'You were signed out because your password or authenticator changed.',
		revoked: 'This session was ended from another device.',
		ended: 'Your session ended or you are not signed in.'
	};
	const reason = $derived(page.url.searchParams.get('reason') ?? 'ended');
	const text = $derived(reasons[reason] ?? reasons.ended);
</script>

<svelte:head>
	<title>Signed out · ServerDock</title>
</svelte:head>

<main class="card">
	<h1 tabindex="-1">Signed out</h1>
	<p id="why">{text}</p>
	<p><a class="button" href="/login">Sign in again</a></p>
</main>

<style>
	.card {
		max-width: 28rem;
		margin: 3rem auto;
		padding: 1.5rem;
		border: 1px solid var(--border);
		border-radius: 0.75rem;
		background: var(--surface);
	}
	.button {
		display: inline-flex;
		align-items: center;
		min-height: var(--target);
		padding: 0.5rem 1rem;
		border-radius: 0.5rem;
		background: var(--accent);
		color: var(--accent-text);
		text-decoration: none;
		font-weight: 600;
	}
</style>
