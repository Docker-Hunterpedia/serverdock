<script lang="ts">
	import { tick } from 'svelte';
	import { flash } from '../../lib/flow.svelte';
	import PageHeading from '../../lib/PageHeading.svelte';

	// Shown once, right after signing in; never from storage.
	let notice = $state(flash.notice);
	let unrecorded = $state(flash.audit === 'unrecorded');
	flash.notice = undefined;
	flash.audit = undefined;
	// A dismissed banner takes its button with it, so focus moves to the heading.
	async function dismiss(hide: () => void) {
		hide();
		await tick();
		document.querySelector<HTMLElement>('h1[tabindex="-1"]')?.focus();
	}
	const failures = $derived(notice?.totp_failures ?? 0);
	const when = $derived(notice?.last_failure_at ? new Date(notice.last_failure_at * 1000).toISOString().slice(0, 16).replace('T', ' ') + ' UTC' : '');
</script>

<svelte:head>
	<title>Overview · ServerDock</title>
</svelte:head>

<PageHeading>Overview</PageHeading>

{#if unrecorded}
	<div role="alert" class="banner">
		<p><span aria-hidden="true">▲</span> The audit record for this action could not be written. It went through, but check the server's disk and logs.</p>
		<button type="button" onclick={() => dismiss(() => (unrecorded = false))}>Dismiss audit warning</button>
	</div>
{/if}
{#if failures > 0}
	<div role="status" class="banner">
		<p>
			{failures} wrong two-factor {failures === 1 ? 'code was' : 'codes were'} entered since your last sign-in{when ? `, the latest at ${when}` : ''}{notice?.last_failure_ip ? ` from ${notice.last_failure_ip}` : ''}.
			If that was not you, change your password.
		</p>
		<button type="button" onclick={() => dismiss(() => (notice = undefined))}>Dismiss sign-in notice</button>
	</div>
{/if}

<section class="empty" aria-labelledby="empty-title">
	<h2 id="empty-title">No services yet</h2>
	<p>Deploying from an image arrives in the next version.</p>
	<p><a href="https://github.com/Docker-Hunterpedia/serverdock/tree/main/docs" rel="noopener">Read the documentation (opens GitHub)</a></p>
</section>

<style>
	.empty {
		border: 1px dashed var(--border);
		border-radius: 0.75rem;
		padding: 1.5rem;
		max-width: 40rem;
	}
	.empty p {
		margin: 0 0 0.75rem;
	}
</style>
