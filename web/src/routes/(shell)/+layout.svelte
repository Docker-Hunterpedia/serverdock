<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { onMount, tick } from 'svelte';
	import { logout, signedOutReason } from '../../lib/api';
	import { guard } from '../../lib/flow.svelte';
	import { startStatusPolling, type Status } from '../../lib/status';

	let { children } = $props();

	let menuOpen = $state(false);
	let menuButton: HTMLButtonElement | undefined = $state();
	let mainEl: HTMLElement | undefined = $state();
	let status: Status = $state({ state: 'unknown' });

	const path = $derived(page.url.pathname);
	const onOverview = $derived(path === '/');
	const onSettings = $derived(path.startsWith('/settings'));

	// Closing the phone menu with Escape returns focus to its button.
	async function closeMenu(refocus: boolean) {
		menuOpen = false;
		if (refocus) {
			await tick();
			menuButton?.focus();
		}
	}
	function onKeydown(e: KeyboardEvent) {
		if (e.key === 'Escape' && menuOpen) {
			e.preventDefault();
			void closeMenu(true);
		}
	}
	function skip(e: MouseEvent) {
		e.preventDefault();
		mainEl?.focus();
		mainEl?.scrollIntoView();
	}

	async function signOut() {
		// Unsaved recovery codes are shown once: ask before ending the session, not after.
		if (guard.unsavedCodes && !confirm('You have new recovery codes on screen that are not marked as saved. If you sign out now they are lost. Sign out anyway?')) return;
		guard.unsavedCodes = false; // a deliberate choice: the recovery-code page lets this navigation through
		try {
			await logout();
		} catch {
			/* the server clears the cookie even when the audit log is down; go on */
		}
		await goto('/signed-out?reason=signed_out');
	}

	onMount(() => {
		const poller = startStatusPolling(
			(s) => (status = s),
			(code) => void goto(`/signed-out?reason=${signedOutReason(code)}`)
		);
		return () => poller.stop();
	});

	</script>

<svelte:window onkeydown={onKeydown} />

<a class="skip" href="#main" onclick={skip}>Skip to main content</a>

<div class="shell">
	<header class="bar">
		<span class="brand">ServerDock</span>
		<button
			bind:this={menuButton}
			type="button"
			class="menu-button"
			aria-expanded={menuOpen}
			aria-controls="main-nav"
			onclick={() => (menuOpen = !menuOpen)}
		>
			<span aria-hidden="true">{menuOpen ? '✕' : '☰'}</span> Menu
		</button>
	</header>

	<nav id="main-nav" class="nav" class:open={menuOpen} aria-label="Main">
		<span class="brand side-brand">ServerDock</span>
		<ul>
			<li><a href="/" aria-current={onOverview ? 'page' : undefined} onclick={() => closeMenu(false)}>Overview</a></li>
			<li>
				<a href="/settings/security" aria-current={onSettings ? 'page' : undefined} onclick={() => closeMenu(false)}>Settings</a>
			</li>
			<li><button type="button" class="signout" onclick={signOut}>Sign out</button></li>
		</ul>
	</nav>

	<main id="main" tabindex="-1" bind:this={mainEl}>
		{@render children()}
	</main>

	<footer class="foot">
		<!-- Not a live region: it updates every 15 s and must not interrupt anyone. -->
		<p>
			ServerDock itself ·
			{#if status.state === 'ok'}
				{status.rssMB.toFixed(0)} MB RAM ·
				{#if status.cpuPercent === null}CPU <span aria-hidden="true">◌</span> measuring{:else}{status.cpuPercent.toFixed(1)}% CPU{/if} · {status.version}
			{:else}
				<span class="unknown"><span aria-hidden="true">◌</span> unknown</span>
			{/if}
		</p>
	</footer>
</div>

<style>
	.shell {
		display: grid;
		grid-template-columns: 15rem 1fr;
		grid-template-rows: 1fr auto;
		grid-template-areas:
			'nav main'
			'nav foot';
		min-height: 100vh;
		min-height: 100dvh;
	}
	.bar {
		display: none;
	}
	.nav {
		grid-area: nav;
		background: var(--surface);
		border-right: 1px solid var(--border);
		padding: 1rem;
	}
	.brand {
		font-weight: 700;
	}
	.side-brand {
		display: block;
		margin-bottom: 1rem;
		min-height: var(--target);
		line-height: var(--target);
	}
	ul {
		list-style: none;
		margin: 0;
		padding: 0;
		display: grid;
		gap: 0.25rem;
	}
	.nav a {
		display: flex;
		align-items: center;
		min-height: var(--target);
		padding: 0 0.75rem;
		border-radius: 0.5rem;
		color: var(--text);
		text-decoration: none;
	}
	.signout {
		width: 100%;
		text-align: left;
		background: none;
		border: 0;
		padding: 0 0.75rem;
		min-height: var(--target);
	}
	.nav a:hover,
	.signout:hover {
		text-decoration: underline;
	}
	.nav a[aria-current='page'] {
		font-weight: 700;
		border: 1px solid var(--border);
		background: var(--bg);
	}
	main {
		grid-area: main;
		padding: 1.5rem;
		min-width: 0;
	}
	main:focus {
		outline: none;
	}
	.foot {
		grid-area: foot;
		border-top: 1px solid var(--border);
		background: var(--surface);
		color: var(--muted);
		padding: 0.75rem 1.5rem;
		font-size: 0.875rem;
	}
	.foot p {
		margin: 0;
	}
	.skip {
		position: absolute;
		left: 0.5rem;
		top: -4rem;
		z-index: 20;
		background: var(--bg);
		color: var(--text);
		border: 2px solid var(--border);
		border-radius: 0.5rem;
		padding: 0.75rem 1rem;
		min-height: var(--target);
	}
	.skip:focus {
		top: 0.5rem;
	}

	@media (max-width: 47.99rem) {
		.shell {
			grid-template-columns: 1fr;
			grid-template-rows: auto auto 1fr auto;
			grid-template-areas:
				'bar'
				'nav'
				'main'
				'foot';
		}
		.bar {
			grid-area: bar;
			display: flex;
			align-items: center;
			justify-content: space-between;
			position: sticky;
			top: 0;
			z-index: 10;
			min-height: var(--bar-height);
			padding: 0 0.75rem 0 1rem;
			background: var(--surface);
			border-bottom: 1px solid var(--border);
		}
		.menu-button {
			background: var(--bg);
		}
		.nav {
			display: none;
			border-right: 0;
			border-bottom: 1px solid var(--border);
		}
		.nav.open {
			display: block;
		}
		.side-brand {
			display: none;
		}
		main {
			padding: 1rem;
		}
	}
</style>
