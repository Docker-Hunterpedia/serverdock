<script lang="ts" generics="Row">
	// Tables (A9): above 768 px a real <table> with a caption and <th scope>; below,
	// the same rows as stacked cards (each card a list of labelled values). Both are
	// in the DOM; CSS shows one, so each is exposed with the right semantics.
	import type { Snippet } from 'svelte';

	type Column = { key: string; label: string };
	type Props = {
		caption: string;
		columns: Column[];
		rows: Row[];
		rowKey: (r: Row) => string | number;
		rowLabel: (r: Row) => string;
		cell: Snippet<[Row, string]>;
		empty: string;
	};
	let { caption, columns, rows, rowKey, rowLabel, cell, empty }: Props = $props();
</script>

{#if rows.length === 0}
	<p class="empty-note">{empty}</p>
{:else}
	<table>
		<caption>{caption}</caption>
		<thead>
			<tr>{#each columns as c, i (c.key)}<th scope="col" class:first={i === 0}>{c.label}</th>{/each}</tr>
		</thead>
		<tbody>
			{#each rows as r (rowKey(r))}
				<tr data-row={rowKey(r)}>
					{#each columns as c, i (c.key)}
						{#if i === 0}<th scope="row">{@render cell(r, c.key)}</th>{:else}<td>{@render cell(r, c.key)}</td>{/if}
					{/each}
				</tr>
			{/each}
		</tbody>
	</table>
	<ul class="cards" role="list" aria-label={caption}>
		{#each rows as r (rowKey(r))}
			<li role="listitem" data-row={rowKey(r)}>
				<p class="card-title">{@render cell(r, columns[0].key)}</p>
				<dl>
					{#each columns.slice(1) as c (c.key)}
						<div><dt>{c.label}</dt><dd>{@render cell(r, c.key)}</dd></div>
					{/each}
				</dl>
			</li>
		{/each}
	</ul>
{/if}

<style>
	table {
		width: 100%;
		border-collapse: collapse;
		margin: 0 0 1rem;
	}
	caption {
		text-align: left;
		font-weight: 700;
		padding: 0.5rem 0;
	}
	th,
	td {
		text-align: left;
		vertical-align: top;
		padding: 0.5rem 0.75rem;
		border-bottom: 1px solid var(--border);
		overflow-wrap: anywhere;
	}
	thead th {
		background: var(--surface);
	}
	.cards {
		display: none;
		list-style: none;
		margin: 0 0 1rem;
		padding: 0;
		gap: 0.75rem;
	}
	.cards li {
		border: 1px solid var(--border);
		border-radius: 0.5rem;
		padding: 0.75rem 1rem;
		background: var(--surface);
	}
	.card-title {
		margin: 0 0 0.5rem;
		font-weight: 700;
		overflow-wrap: anywhere;
	}
	dl {
		margin: 0;
	}
	dl div {
		display: grid;
		grid-template-columns: 8rem 1fr;
		gap: 0.5rem;
		padding: 0.25rem 0;
	}
	dt {
		color: var(--muted);
	}
	dd {
		margin: 0;
		overflow-wrap: anywhere;
	}
	.empty-note {
		margin: 0 0 1rem;
		color: var(--muted);
	}
	@media (max-width: 47.99rem) {
		table {
			display: none;
		}
		.cards {
			display: grid;
		}
	}
</style>
