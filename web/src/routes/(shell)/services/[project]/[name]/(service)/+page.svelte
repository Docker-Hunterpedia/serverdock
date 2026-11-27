<script lang="ts">
	import ConfirmDialog from '../../../../../../lib/ConfirmDialog.svelte';
	import ResponsiveTable from '../../../../../../lib/ResponsiveTable.svelte';
	import { runAction } from '../../../../../../lib/api';
	import { queuedMessage, useService } from '../../../../../../lib/servicectx.svelte';
	import { KIND_WORDS, canRollBackTo, deployPath, deployResult, stagesSummary, strategyConsequence, triggeredBy, type DeployItem, type QueuedDeploy } from '../../../../../../lib/services';
	import { absolute } from '../../../../../../lib/time';

	const ctx = useService();
	let dialog: ConfirmDialog | undefined = $state();
	let busy = $state(false);

	const columns = [
		{ key: 'number', label: 'Deploy' },
		{ key: 'result', label: 'Result' },
		{ key: 'image', label: 'Image' },
		{ key: 'by', label: 'Triggered by' },
		{ key: 'stages', label: 'Stages' },
		{ key: 'when', label: 'When' },
		{ key: 'actions', label: 'Actions' }
	];

	async function rollBack(d: DeployItem) {
		const svc = ctx.service;
		if (!svc || busy) return;
		const ok = await dialog?.ask({
			title: `Roll back ${ctx.ref} to #${d.number}?`,
			lines: [
				`Starts a new deploy that runs the image of deploy #${d.number} (${d.image_ref}) with the settings it had then. The deploy history keeps every deploy; this one is added as a new rollback.`,
				strategyConsequence(svc.strategy),
				'Only deploys that were once live can be rolled back to. If the image is no longer on this server, the rollback fails and the live version keeps serving.'
			],
			confirm: `Roll back to #${d.number}`
		});
		if (!ok) return;
		busy = true;
		ctx.clearMessages();
		try {
			const q = await runAction<QueuedDeploy>('services.rollback', { service: ctx.ref, to: d.number });
			await ctx.load(false);
			ctx.say(queuedMessage(`Rollback to #${d.number}`, q));
		} catch (e) {
			await ctx.fail(e);
		} finally {
			busy = false;
		}
	}
</script>

<ConfirmDialog bind:this={dialog} />

<section aria-labelledby="deploys-h">
	<h2 id="deploys-h">Deploys</h2>
	{#if ctx.deploys && ctx.deploys.length === 0}
		<p class="note">No deploys yet. Use Deploy above to run this service for the first time.</p>
	{:else if ctx.deploys}
		<p class="note">Times are UTC. A queued or running deploy changes only when you use Refresh; its page shows more.</p>
		<ResponsiveTable
			caption="Deploys of {ctx.ref}, newest first"
			{columns}
			rows={ctx.deploys}
			rowKey={(r: DeployItem) => r.number}
			rowLabel={(r: DeployItem) => `Deploy #${r.number}`}
			empty="No deploys."
		>
			{#snippet cell(r: DeployItem, key: string)}
				{#if key === 'number'}
					#{r.number}{#if r.kind !== 'deploy'} <span class="kind">({KIND_WORDS[r.kind] ?? r.kind})</span>{/if}
				{:else if key === 'result'}
					{@const res = deployResult(r)}
					<span><span aria-hidden="true">{res.symbol}</span> {res.word}</span>
					{#if r.status === 'failed' && r.reason}<span class="reason">{r.reason}</span>{/if}
				{:else if key === 'image'}
					<code>{r.image_ref || 'unknown'}</code>
				{:else if key === 'by'}
					{triggeredBy(r)}
				{:else if key === 'stages'}
					{stagesSummary(r)}
				{:else if key === 'when'}
					{absolute(r.created_at)}
				{:else if key === 'actions'}
					<span class="acts">
						<a href={deployPath(ctx.project, ctx.name, r.number)} aria-label="Details of deploy #{r.number}">Details</a>
						{#if canRollBackTo(r)}
							<button type="button" onclick={() => rollBack(r)} aria-disabled={busy ? 'true' : undefined}>Roll back to #{r.number}</button>
						{/if}
					</span>
				{/if}
			{/snippet}
		</ResponsiveTable>
	{:else}
		<p class="note">Loading the deploys…</p>
	{/if}
</section>

<style>
	.kind {
		color: var(--muted);
	}
	.reason {
		display: block;
		color: var(--muted);
		font-size: 0.9375rem;
	}
	.acts {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
		align-items: center;
	}
</style>
