<script lang="ts">
	import { goto } from '$app/navigation';
	import { onDestroy, tick } from 'svelte';
	import { ApiError, login, NETWORK_MESSAGE } from '../../../lib/api';
	import { Countdown } from '../../../lib/countdown.svelte';
	import { flow } from '../../../lib/flow.svelte';
	import Alerts from '../../../lib/Alerts.svelte';
	import Field from '../../../lib/Field.svelte';
	import PageHeading from '../../../lib/PageHeading.svelte';

	let username = $state(flow.username);
	let password = $state('');
	let busy = $state(false);
	let error: string | null = $state(null);
	let links: { id: string; text: string }[] = $state([]);
	let status = $state('');
	let fieldError: Record<string, string> = $state({});
	let alerts: Alerts | undefined = $state();
	const countdown = new Countdown((m) => (status = m));
	onDestroy(() => countdown.stop());
	const locked = $derived(busy || countdown.active);

	async function fail(msg: string, l: { id: string; text: string }[] = []) {
		error = msg;
		links = l;
		await tick();
		alerts?.focus();
	}

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		if (locked) return;
		error = null;
		links = [];
		fieldError = {};
		status = '';
		const problems: { id: string; text: string }[] = [];
		if (!username.trim()) {
			fieldError.username = 'Enter your username.';
			problems.push({ id: 'username', text: 'Username' });
		}
		if (!password) {
			fieldError.password = 'Enter your password.';
			problems.push({ id: 'password', text: 'Password' });
		}
		if (problems.length) return fail('Fill in both fields.', problems);
		busy = true;
		status = 'Signing in…';
		try {
			const r = await login(username.trim(), password);
			flow.username = username.trim();
			flow.pendingUntil = Date.now() + r.expires_in * 1000;
			password = '';
			await goto('/login/2fa');
		} catch (err) {
			status = '';
			password = ''; // passwords are never kept after a failure; the username is
			if (!(err instanceof ApiError)) throw err;
			if (err.code === 'network') await fail(NETWORK_MESSAGE);
			else if (err.code === 'rate_limited') countdown.start(err.retryAfter ?? 60);
			else if (err.code === 'busy' || err.code === 'audit_unavailable') await fail('The server is busy. Try again in a moment. Nothing was changed.');
			else await fail(err.message, err.code === 'invalid_credentials' ? [{ id: 'password', text: 'Password' }] : []);
		} finally {
			busy = false;
		}
	}
</script>

<svelte:head>
	<title>Sign in · ServerDock</title>
</svelte:head>

<PageHeading>Sign in</PageHeading>

<Alerts bind:this={alerts} {error} {links} {status} />
{#if countdown.active}
	<p class="banner" aria-hidden="true">Try again in {countdown.text}</p>
{/if}

<form onsubmit={submit} aria-busy={busy} novalidate>
	<Field id="username" label="Username" bind:value={username} autocomplete="username" error={fieldError.username} />
	<Field id="password" label="Password" bind:value={password} type="password" autocomplete="current-password" error={fieldError.password} />
	<div class="actions">
		<button type="submit" class="primary" aria-disabled={locked}>Sign in</button>
	</div>
</form>
