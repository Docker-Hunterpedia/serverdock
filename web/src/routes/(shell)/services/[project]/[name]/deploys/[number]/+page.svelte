<script lang="ts">
	// The Deploy detail page (plan 0004 step 14): what a deploy did, its stages, the pull's
	// progress, the container log and the actions that apply. While the deploy runs and
	// the page is visible it asks deploys.get every 2 s (5 s once a pull has run 2 minutes);
	// the polling lives in DeployWatch. Polling never moves focus; one status region
	// announces stage changes and success, one alert announces a failure, once.
	import { beforeNavigate, goto } from '$app/navigation';
	import { page } from '$app/state';
	import { onDestroy, tick, untrack } from 'svelte';
	import Alerts from '../../../../../../../lib/Alerts.svelte';
	import ConfirmDialog from '../../../../../../../lib/ConfirmDialog.svelte';
	import PageHeading from '../../../../../../../lib/PageHeading.svelte';
	import { ApiError, Cancelled, runAction, signedOutReason } from '../../../../../../../lib/api';
	import { DeployWatch } from '../../../../../../../lib/deploywatch.svelte';
	import { takeCreated, type Created } from '../../../../../../../lib/flow.svelte';
	import {
		KIND_WORDS,
		SEC6_DOCS_URL,
		appSafetyText,
		cancelDeploy,
		cancelReason,
		canRollBackTo,
		deployPath,
		deployResult,
		isUnfinished,
		mutationMessage,
		newContainerText,
		pullText,
		rollbackConsequence,
		servicePath,
		sharesPanelText,
		splitLog,
		stageLabel,
		stageView,
		strategyConsequence,
		strategyWord,
		tailLines,
		triggeredBy,
		currentStage,
		isRetrying,
		hasStarted,
		sentence,
		supersededText
	} from '../../../../../../../lib/services';
	import type { QueuedDeploy } from '../../../../../../../lib/services';
	import { absolute } from '../../../../../../../lib/time';

	const project = $derived(page.params.project ?? '');
	const name = $derived(page.params.name ?? '');
	const number = $derived(Number(page.params.number));
	const ref = $derived(`${project}/${name}`);

	const watch = new DeployWatch();
	let alerts: Alerts | undefined = $state();
	let dialog: ConfirmDialog | undefined = $state();
	let busy = $state(false);
	let fullLog = $state(false);

	// What "Create and deploy" just stored for this deploy, taken once per deploy: the
	// page reacts to a change of the address (another deploy of the same page), so a
	// notice or warning of one deploy is never shown for another or twice.
	let lastKey = `${page.params.project}/${page.params.name}#${page.params.number}`;
	let just: Created | undefined = $state(takeCreated(`${page.params.project}/${page.params.name}`, Number(page.params.number)));
	const warnings = $derived(just?.warnings ?? []);

	let opened = '';
	$effect(() => {
		const key = `${ref}#${number}`;
		untrack(() => {
			if (key === opened) return;
			opened = key;
			if (key !== lastKey) {
				lastKey = key;
				just = takeCreated(ref, number);
			}
			fullLog = false;
			busy = false;
			watch.focusHeading = () => document.querySelector<HTMLElement>('h1[tabindex="-1"]')?.focus();
			if (Number.isInteger(number) && number >= 1) watch.open(ref, number);
			else {
				watch.destroy();
				watch.notFound = true;
				watch.loading = false;
			}
		});
	});

	// Hidden: no polling and no announcement. Visible again: refreshed first, then announced.
	$effect(() => {
		const onVisibility = () => watch.setVisible(document.visibilityState !== 'hidden');
		document.addEventListener('visibilitychange', onVisibility);
		return () => document.removeEventListener('visibilitychange', onVisibility);
	});
	onDestroy(() => watch.destroy());
	beforeNavigate(() => dialog?.close());

	const d = $derived(watch.data?.deploy);
	const unfinished = $derived(d ? isUnfinished(d.status) : false);
	const result = $derived(d ? deployResult(d) : undefined);
	const reason = $derived(watch.data ? cancelReason(watch.data, watch.cancelAsked) : null);
	const staleWhy = 'The status is unknown right now: this page could not reach the server. Use Check again.';
	const stage = $derived(watch.data ? currentStage(watch.data) : '');
	const pulling = $derived(!!watch.data && unfinished && d?.stage === 'pull' && watch.data.stages.find((s) => s.stage === 'pull')?.state === 'running');
	const safety = $derived(d && watch.data ? appSafetyText(d, watch.previous, hasStarted(watch.data)) : undefined);
	/** What is shown may be out of date: nothing on the page can be acted on until the server answers again. */
	const stale = $derived(watch.unknown || !!watch.refused);
	const retrying = $derived(watch.data?.stages.find((s) => isRetrying(s)));
	const strategy = $derived(d?.strategy || 'blue_green');
	const logParts = $derived(watch.log.kind === 'text' ? splitLog(watch.log.log.text ?? '') : undefined);

	async function say(error: string) {
		watch.status = '';
		watch.alert = error;
		await tick();
		alerts?.focus(); // an action the user took failed: the summary is read and reachable
	}

	async function fail(e: unknown) {
		if (e instanceof Cancelled) return;
		if (!(e instanceof ApiError)) throw e;
		if (e.status === 401) {
			await goto(`/signed-out?reason=${signedOutReason(e.code)}`);
			return;
		}
		await say(mutationMessage(e));
	}

	async function cancel() {
		if (!d || busy || reason || stale) return;
		const forRef = ref;
		const n = number;
		busy = true;
		watch.alert = null;
		void watch.announce('Cancel request sent, waiting for the answer…');
		try {
			const out = await cancelDeploy(forRef, n);
			if (forRef !== ref || n !== number) return;
			if (out.status === 'cancelling') {
				watch.cancelAsked = true;
				void watch.announce('Cancel requested. The deploy stops at its next safe point.');
			} else {
				watch.status = '';
			}
			await watch.refresh(); // the refresh says "cancelled" when the deploy has ended
		} catch (e) {
			if (forRef === ref && n === number) await fail(e);
		} finally {
			busy = false;
		}
	}

	async function redeploy() {
		if (!d || busy || stale) return;
		const forRef = ref;
		// Redeploy always reuses the service's live deploy: ask again first, so the dialog names the one that is live now.
		busy = true;
		const fresh = await watch.refresh();
		busy = false;
		if (forRef !== ref) return;
		if (!fresh) {
			await say('Could not check which version is live, so nothing was started. Use Check again, then try again.');
			return;
		}
		const cur = watch.data?.deploy;
		if (!cur || !cur.active) {
			await say(`Deploy #${number} is no longer the live version, so Redeploy was not started. Redeploy it from the service page.`);
			return;
		}
		const ok = await dialog?.ask({
			title: `Redeploy ${forRef}?`,
			lines: [
				`Redeploys the live version of ${forRef}: deploy #${cur.number} (${cur.image_ref}). It starts a new deploy that reuses that exact image with its settings, in a fresh container. Nothing is pulled again.`,
				strategyConsequence(cur.strategy || 'blue_green')
			],
			confirm: 'Redeploy'
		});
		if (!ok || forRef !== ref) return;
		await queue('Redeploy request sent, waiting for the answer…', () => runAction<QueuedDeploy>('services.redeploy', { service: forRef }), forRef);
	}

	async function rollBack() {
		if (!d || busy || stale) return;
		const forRef = ref;
		const cur = d;
		const ok = await dialog?.ask({
			title: `Roll back ${forRef} to #${cur.number}?`,
			lines: [
				`Starts a new deploy that runs the image of deploy #${cur.number} (${cur.image_ref}) with the settings it had then. The deploy history keeps every deploy; this one is added as a new rollback.`,
				rollbackConsequence(cur.strategy || 'blue_green', cur.number),
				'Only deploys that were once live can be rolled back to. If the image is no longer on this server, the rollback fails and the live version keeps serving.'
			],
			confirm: `Roll back to #${cur.number}`
		});
		if (!ok || forRef !== ref) return;
		await queue('Rollback request sent, waiting for the answer…', () => runAction<QueuedDeploy>('services.rollback', { service: forRef, to: cur.number }), forRef);
	}

	/** Queues the work, then opens the new deploy's page: it is queued, not done. */
	async function queue(label: string, run: () => Promise<QueuedDeploy>, forRef: string) {
		busy = true;
		watch.alert = null;
		void watch.announce(label);
		try {
			const q = await run();
			if (forRef !== ref) return;
			await goto(deployPath(project, name, q.deploy));
		} catch (e) {
			if (forRef === ref) await fail(e);
		} finally {
			busy = false;
		}
	}

	function toggleFull() {
		fullLog = !fullLog;
		void watch.announce(fullLog ? 'Full log shown.' : 'Last 40 lines shown.');
	}

	function skipLog(e: MouseEvent) {
		e.preventDefault();
		document.getElementById('show-full-log')?.focus();
	}
</script>

<svelte:head>
	<title>{watch.notFound ? 'Deploy not found' : `Deploy #${page.params.number}`} · {ref} · ServerDock</title>
</svelte:head>

<ConfirmDialog bind:this={dialog} />

{#if watch.notFound}
	<PageHeading>Deploy not found</PageHeading>
	<p>There is no deploy #{page.params.number} of <a href={servicePath({ project, name })}>{ref}</a>. The service may have been deleted.</p>
	<p><a class="button" href="/services">Back to Services</a></p>
{:else}
	<PageHeading>
		Deploy #{page.params.number}
		{#if !watch.loading || watch.unknown}
			<span class="state" data-testid="deploy-status">
				{#if watch.unknown || !result}<span aria-hidden="true">◌</span> Unknown{:else}<span aria-hidden="true">{result.symbol}</span> {result.word}{/if}
			</span>
		{/if}
	</PageHeading>
	<p class="note"><a href="/services">Services</a> / <a href={servicePath({ project, name })}>{ref}</a> / Deploy #{page.params.number}</p>

	{#if just}
		<p class="note">This deploy was just queued and runs in the background.</p>
		{#if just.applied === false}
			<div class="banner" data-testid="not-applied">
				<p><span aria-hidden="true">▲</span> {just.notice || 'Saved, but not applied yet: the public port is not being served yet.'}</p>
			</div>
		{/if}
	{/if}
	{#each warnings as w (w.code)}
		<div class="banner">
			<p>
				<span aria-hidden="true">▲</span>
				{w.code === 'shares_panel_address' ? sharesPanelText(w.message, just?.port ?? null) : w.message}
				{#if w.code === 'shares_panel_address'}<a href={SEC6_DOCS_URL} target="_blank" rel="noopener noreferrer">Read about this (opens GitHub)</a>{/if}
			</p>
		</div>
	{/each}

	<Alerts error={watch.alert} status={watch.status} bind:this={alerts} />

	{#if watch.refused}
		<p class="note" data-testid="status-refused"><span aria-hidden="true">▲</span> The server refused to show this deploy: {watch.refused} This page does not ask again.</p>
	{:else if watch.unknown || (!d && !watch.loading)}
		<div data-vanishes>
			<p class="note" data-testid="status-unknown"><span aria-hidden="true">◌</span> Status unknown: could not reach the server.{#if d} What is shown below may be out of date.{/if}</p>
			<p class="hint">This page asks again by itself while it is open; you can also ask now.</p>
			<p><button type="button" data-testid="check-again" onclick={() => void watch.refresh(true)}>Check again</button></p>
		</div>
	{:else if !d}
		<p class="note">Loading the deploy…</p>
	{/if}

	{#if d && watch.data && safety}
		<section aria-labelledby="h-what" class="block" data-testid="what-happened">
			<h2 id="h-what">What happened</h2>
			<dl>
				<dt>Result</dt>
				<dd data-testid="deploy-result"><span aria-hidden="true">{result?.symbol}</span> {result?.word}</dd>
				<dt>Your app</dt>
				<dd data-testid="app-safety"><span aria-hidden="true">{safety.symbol}</span> {safety.text}</dd>
				<dt>New container</dt>
				<dd data-testid="new-container">{newContainerText(watch.data)}</dd>
				{#if d.status === 'superseded'}
					<dt>Reason</dt>
					<dd data-testid="deploy-reason">{supersededText(watch.data)}</dd>
				{:else if d.status === 'failed' || (d.reason && d.status !== 'cancelled')}
					<dt>Reason</dt>
					<dd data-testid="deploy-reason">{d.reason ? sentence(d.reason) : 'No reason was recorded.'}</dd>
				{:else if d.status === 'cancelled'}
					<dt>Reason</dt>
					<dd data-testid="deploy-reason">It was cancelled.</dd>
				{/if}
			</dl>
			{#if d.reason_code === 'disk_full'}
				<p class="warn" data-testid="disk-full"><span aria-hidden="true">▲</span> The server's disk is full. Free some space on the server (for example remove unused images and stopped containers), then deploy again. The app was not changed by running out of space.</p>
			{/if}
			{#if d.reason_code === 'interrupted'}
				<p class="warn" data-testid="interrupted"><span aria-hidden="true">◌</span> ServerDock stopped while this deploy was running, so it cannot say how far it got. Check the service, then deploy again if needed.</p>
			{/if}
			{#if retrying && unfinished}
				<p class="warn" data-testid="waiting-retry"><span aria-hidden="true">○</span> {stageLabel(retrying.stage)} did not succeed and is waiting to retry. The reason is in the stage list below.</p>
			{/if}
		</section>

		<section aria-labelledby="h-this" class="block" data-testid="this-deploy">
			<h2 id="h-this">This deploy</h2>
			<dl>
				<dt>Kind</dt>
				<dd>{KIND_WORDS[d.kind] ?? d.kind}</dd>
				<dt>Image</dt>
				<dd><code>{d.image_ref || 'unknown'}</code></dd>
				{#if d.repo_digest}<dt>Digest</dt><dd><code>{d.repo_digest}</code></dd>{/if}
				{#if d.image_id}<dt>Image ID</dt><dd><code>{d.image_id}</code></dd>{/if}
				<dt>Strategy</dt>
				<dd>{strategyWord(strategy)}</dd>
				<dt>Triggered by</dt>
				<dd>{triggeredBy(d)}</dd>
				<dt>Created</dt>
				<dd>{absolute(d.created_at)}</dd>
				{#if d.started_at}<dt>Started</dt><dd>{absolute(d.started_at)}</dd>{/if}
				{#if d.finished_at}<dt>Finished</dt><dd>{absolute(d.finished_at)}</dd>{/if}
			</dl>
		</section>

		<section aria-labelledby="h-stages" class="block">
			<h2 id="h-stages">Stages</h2>
			{#if watch.data.stages.length === 0}
				<p data-testid="no-stages">{d.status === 'queued' ? 'No stage has started: this deploy is queued and starts when the work ahead of it on this service has finished.' : 'No stage ran.'}</p>
			{:else}
				<ol class="stages" data-testid="stages">
					{#each watch.data.stages as s (s.stage)}
						{@const v = stageView(s, !unfinished)}
						<li data-testid="stage-{s.stage}" data-state={s.state} aria-current={unfinished && s.stage === stage ? 'step' : undefined}>
							<span class="stage-name">{stageLabel(s.stage)}</span>: <span class="stage-state"><span aria-hidden="true">{v.symbol}</span> {v.word}</span>
							{#if s.detail}<span class="detail">{s.detail}</span>{/if}
						</li>
					{/each}
				</ol>
			{/if}

			{#if pulling && !retrying}
				{@const p = watch.data.progress}
				{@const known = !!p && p.bytes_total > 0}
				<div class="pull" data-testid="pull-progress">
					{#if known}
						<label for="pull-bar">Image pull progress</label>
						<progress id="pull-bar" aria-describedby="pull-text" max={p.bytes_total} value={p.bytes}></progress>
					{:else}
						<!-- No bar without a size: an animated indeterminate bar cannot be paused (WCAG 2.2.2); the text says it. -->
						<p id="pull-label" class="label">Image pull progress</p>
					{/if}
					<p id="pull-text" data-testid="pull-text">{pullText(p)}</p>
				</div>
			{/if}
		</section>

		{#if unfinished}
			<div class="toolbar" data-vanishes>
				<button type="button" data-testid="cancel-deploy" onclick={cancel} aria-disabled={reason || busy || stale ? 'true' : undefined} aria-describedby={reason || stale ? 'cancel-reason' : undefined}>Cancel deploy</button>
			</div>
			{#if reason || stale}<p id="cancel-reason" class="hint" data-testid="cancel-reason">{stale && !reason ? staleWhy : reason}</p>{/if}
		{:else if d.active || canRollBackTo(d)}
			<div class="toolbar" data-vanishes>
				{#if d.active}
					<button type="button" data-testid="redeploy" onclick={redeploy} aria-disabled={busy || stale ? 'true' : undefined} aria-describedby={stale ? 'act-stale' : undefined}>Redeploy</button>
				{/if}
				{#if canRollBackTo(d)}
					<button type="button" data-testid="rollback" onclick={rollBack} aria-disabled={busy || stale ? 'true' : undefined} aria-describedby={stale ? 'act-stale' : undefined}>Roll back to #{d.number}</button>
				{/if}
			</div>
			{#if stale}<p id="act-stale" class="hint">{staleWhy}</p>{/if}
		{/if}

		<section aria-labelledby="h-log" class="block" data-testid="log-section">
			<h2 id="h-log">Container log ({fullLog ? 'full log' : 'last 40 lines'})</h2>
			{#if unfinished}
				<p class="note" data-testid="log-message">There is no log while the deploy runs: it is kept when the deploy's container is removed.</p>
			{:else if watch.log.kind === 'none' || watch.log.kind === 'loading'}
				<p class="note">Loading the log…</p>
			{:else if watch.log.kind === 'message'}
				<p class="note" data-testid="log-message">{watch.log.message}</p>
			{:else if watch.log.kind === 'busy' || watch.log.kind === 'error'}
				<p class="note" data-testid="log-message">{watch.log.message}</p>
				<p data-vanishes><button type="button" data-testid="log-retry" onclick={() => void watch.loadLog()}>Try again</button></p>
			{:else if logParts}
				{@const out = fullLog ? logParts.output : tailLines(logParts.output, 40)}
				<p class="hint">Container output is shown as plain text, exactly as the container printed it. It can contain secrets: do not share it.</p>
				{#if watch.log.log.truncated}<p class="note" data-testid="log-truncated">The log file holds more than 1 MiB: only the first 1 MiB is shown.</p>{/if}
				<div class="logbox">
					<a class="skip-log" href="#show-full-log" data-testid="skip-log" onclick={skipLog}>Skip the container log</a>
					{#if fullLog && logParts.header}
						<h3 id="h-loghead">Written by ServerDock</h3>
						<!-- svelte-ignore a11y_no_noninteractive_tabindex (a scrollable log must be reachable by keyboard: plan 0004 accessibility rules) -->
						<pre class="log head" tabindex="0" role="group" aria-labelledby="h-loghead" data-testid="log-header">{logParts.header}</pre>
						<h3 id="h-logout">Printed by the container</h3>
					{/if}
					<!-- svelte-ignore a11y_no_noninteractive_tabindex (as above) -->
					<pre class="log" tabindex="0" role="group" aria-labelledby={fullLog && logParts.header ? 'h-logout' : 'h-log'} data-testid={fullLog ? 'log-full' : 'log-tail'}>{out || '(the container printed nothing)'}</pre>
				</div>
				<p>
					<button type="button" id="show-full-log" data-testid="show-full-log" onclick={toggleFull}>{fullLog ? 'Show last 40 lines only' : 'Show full log'}</button>
				</p>
				{#if !fullLog}<p class="hint">The header and stage lines ServerDock wrote are in the full log.</p>{/if}
			{/if}
		</section>
	{/if}

	<p><a class="button" href={servicePath({ project, name })}>Back to {ref}</a> <a class="button" href="/services">Back to Services</a></p>
{/if}

<style>
	.state {
		font-size: 1rem;
		margin-left: 0.5rem;
		white-space: nowrap;
	}
	.block {
		margin: 0 0 1.5rem;
		max-width: 48rem;
	}
	dl {
		display: grid;
		grid-template-columns: max-content minmax(0, 1fr);
		gap: 0.25rem 1rem;
		margin: 0 0 0.75rem;
	}
	dt {
		font-weight: 600;
	}
	dd {
		margin: 0;
		overflow-wrap: anywhere;
	}
	@media (max-width: 30rem) {
		dl {
			grid-template-columns: minmax(0, 1fr);
		}
		dd {
			margin-bottom: 0.5rem;
		}
	}
	.stages {
		margin: 0 0 1rem;
		padding-left: 1.5rem;
	}
	.stages li {
		margin: 0.25rem 0;
		overflow-wrap: anywhere;
	}
	.stages li[aria-current='step'] .stage-name {
		font-weight: 700;
	}
	.stage-state {
		white-space: nowrap;
	}
	.detail {
		display: block;
		color: var(--muted);
		font-size: 0.9375rem;
	}
	.pull progress {
		display: block;
		width: min(100%, 24rem);
		height: 1rem;
		margin: 0.25rem 0;
	}
	.pull p {
		margin: 0 0 0.5rem;
		color: var(--muted);
	}
	.warn {
		font-weight: 600;
		border-left: 4px solid var(--border);
		padding-left: 0.75rem;
	}
	pre.log {
		margin: 0 0 0.75rem;
		padding: 0.75rem;
		max-height: 20rem;
		overflow: auto; /* scrolls sideways: long lines are not wrapped or cut */
		white-space: pre;
		font-family: var(--mono);
		font-size: 0.875rem;
		background: var(--surface);
		border: 1px solid var(--border);
		border-left: 6px solid var(--border);
		border-radius: 0.375rem;
	}
	pre.log.head {
		border-left-style: dashed;
		max-height: 12rem;
	}
	h3 {
		margin: 0.75rem 0 0.25rem;
		font-size: 1rem;
	}
	/* A skip link past the log: hidden until it has focus. */
	.logbox {
		position: relative;
	}
	.skip-log {
		position: absolute;
		left: -100vw;
	}
	/* Over the log's corner while it has focus: nothing moves. */
	.skip-log:focus {
		left: auto;
		right: 0.5rem;
		top: 0.5rem;
		z-index: 2;
		display: inline-flex;
		align-items: center;
		min-height: var(--target);
		padding: 0 0.75rem;
		background: var(--bg);
		color: var(--text);
		border: 2px solid var(--border);
		border-radius: 0.5rem;
	}
	.label {
		margin: 0;
		font-weight: 600;
	}
</style>
