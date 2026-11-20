<script lang="ts">
	import { goto } from '$app/navigation';
	import { onDestroy, onMount, tick } from 'svelte';
	import { ApiError, NETWORK_MESSAGE, twoFactor } from '../../../../lib/api';
	import { Countdown, humanDuration } from '../../../../lib/countdown.svelte';
	import { flash, flow } from '../../../../lib/flow.svelte';
	import Alerts from '../../../../lib/Alerts.svelte';
	import Field from '../../../../lib/Field.svelte';
	import PageHeading from '../../../../lib/PageHeading.svelte';

	let recoveryMode = $state(false);
	let code = $state('');
	let recoveryCode = $state('');
	let busy = $state(false);
	let expired = $state(false);
	let error: string | null = $state(null);
	let links: { id: string; text: string }[] = $state([]);
	let status = $state('');
	let fieldError: Record<string, string> = $state({});
	let alerts: Alerts | undefined = $state();
	const countdown = new Countdown((m) => (status = m));
	const locked = $derived(busy || countdown.active || expired);
	function refuse(): boolean {
		if (countdown.active) status = `Try again in ${humanDuration(countdown.remaining)}.`;
		return locked;
	}

	// The pending step lasts 5 minutes. About a minute before it ends a polite status
	// says so (focus never moves); at the end the page says to sign in again.
	let warnTimer: ReturnType<typeof setTimeout> | undefined;
	let endTimer: ReturnType<typeof setTimeout> | undefined;
	onMount(() => {
		if (flow.pendingUntil > 0) {
			const left = flow.pendingUntil - Date.now();
			if (left <= 0) return void expire();
			endTimer = setTimeout(expire, left);
			if (left > 60_000) warnTimer = setTimeout(() => (status = 'About a minute left to enter your code.'), left - 60_000);
			else status = 'About a minute left to enter your code.';
		}
	});
	onDestroy(() => {
		clearTimeout(warnTimer);
		clearTimeout(endTimer);
		countdown.stop();
	});
	async function expire() {
		expired = true;
		status = '';
		error = 'This step expired. Sign in again.';
		links = [];
		await tick();
		alerts?.focus(); // the form is gone, so focus must not be left on nothing
	}

	async function toggleMode() {
		recoveryMode = !recoveryMode;
		error = null;
		links = [];
		fieldError = {};
		await tick();
		document.getElementById(recoveryMode ? 'recovery' : 'code')?.focus();
		status = recoveryMode ? 'Enter one of your recovery codes instead.' : 'Enter the code from your authenticator app instead.';
	}

	async function fail(msg: string, l: { id: string; text: string }[] = []) {
		error = msg;
		links = l;
		await tick();
		alerts?.focus();
	}

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		if (refuse()) return;
		error = null;
		links = [];
		fieldError = {};
		status = '';
		const id = recoveryMode ? 'recovery' : 'code';
		const value = (recoveryMode ? recoveryCode : code).replace(/\s/g, '');
		if (!value || (!recoveryMode && !/^\d{6}$/.test(value))) {
			fieldError[id] = recoveryMode ? 'Enter one of your recovery codes.' : 'Enter the 6 digits from your app.';
			return fail(recoveryMode ? 'Enter a recovery code.' : 'Enter the 6-digit code.', [{ id, text: recoveryMode ? 'Recovery code' : 'Code from your authenticator' }]);
		}
		busy = true;
		status = 'Checking…';
		try {
			const r = await twoFactor(recoveryMode ? { recovery_code: value } : { code: value });
			flash.notice = r.notice;
			flash.audit = r.audit;
			flow.pendingUntil = 0;
			await goto('/');
		} catch (err) {
			status = '';
			if (!(err instanceof ApiError)) throw err;
			// Codes are never kept after a failure.
			code = '';
			recoveryCode = '';
			if (err.code === 'network') await fail(NETWORK_MESSAGE);
			else if (err.code === 'code_reused') await fail('That code was already used. Wait for the next one.', [{ id, text: recoveryMode ? 'Recovery code' : 'Code from your authenticator' }]);
			else if (err.code === 'invalid_code') {
				const tries = err.triesLeft !== undefined ? ` ${err.triesLeft} ${err.triesLeft === 1 ? 'try' : 'tries'} left.` : '';
				if (recoveryMode) await fail(`That recovery code is not valid or was already used.${tries}`, [{ id, text: 'Recovery code' }]);
				else await fail(`That code is wrong. Check your device's clock and try the next code.${tries}`, [{ id, text: 'Code from your authenticator' }]);
			}
			else if (err.status === 401 && err.code === 'unauthenticated') {
				expired = true;
				await fail('This step expired or ran out of tries. Sign in again.');
			} else if (err.code === 'rate_limited') countdown.start(err.retryAfter ?? 60);
			else if (err.code === 'busy' || err.code === 'audit_unavailable') await fail('The server is busy. Try again in a moment. Nothing was changed.');
			else await fail(err.message);
		} finally {
			busy = false;
		}
	}
</script>

<svelte:head>
	<title>Two-factor sign-in · ServerDock</title>
</svelte:head>

<PageHeading>Two-factor sign-in</PageHeading>

<p>
	{#if flow.username}Signing in as <strong>{flow.username}</strong>. {/if}
	{#if flow.pendingUntil > 0}You have 5 minutes to finish this step.{:else}This step is time-limited. If it runs out you will be asked to sign in again.{/if}
</p>

<Alerts bind:this={alerts} {error} {links} {status} />
{#if countdown.active}
	<p class="banner">Try again in {countdown.text}</p>
{/if}

{#if expired}
	<p><a class="primary" href="/login">Sign in again</a></p>
{:else}
	<form onsubmit={submit} aria-busy={busy} novalidate>
		{#if recoveryMode}
			<Field
				id="recovery"
				label="Recovery code"
				bind:value={recoveryCode}
				autocomplete="off"
				hint="One of the ten codes you saved at setup, like xxxx-xxxx-xxxx-xxxx. Each works once."
				error={fieldError.recovery}
			/>
		{:else}
			<Field
				id="code"
				label="Code from your authenticator"
				bind:value={code}
				inputmode="numeric"
				autocomplete="one-time-code"
				maxlength={7}
				hint="The 6 digits your app shows now. Nothing is sent until you press Verify."
				error={fieldError.code}
			/>
		{/if}
		<div class="actions">
			<button type="submit" class="primary" aria-disabled={locked}>Verify</button>
			<button type="button" class="link" onclick={toggleMode}>
				{recoveryMode ? 'Use my authenticator app instead' : 'Use a recovery code instead'}
			</button>
		</div>
	</form>
{/if}
