<script lang="ts">
	import { goto, invalidateAll } from '$app/navigation';
	import { navigating, page } from '$app/state';
	import { api } from '$lib/api/client';
	import type { components } from '$lib/api/v1';
	import type { PageData } from './$types';
	import { onMount } from 'svelte';
	import Button from '$lib/components/ui/Button.svelte';
	import EmptyState from '$lib/components/ui/EmptyState.svelte';
	import RepoStarterForm from '$lib/components/app/RepoStarterForm.svelte';
	import { createCanvasPan } from '$lib/actions/canvas-pan.svelte';
	import { Icon } from '@steeze-ui/svelte-icon';
	import { ArrowLeft, Minus, Plus, ServerStack, ArrowsPointingIn } from '@steeze-ui/heroicons';

	type ServerResponse = components['schemas']['ServerResponse'];
	type AnalysisResponse = components['schemas']['AnalyzeSourceResponse'];
	type ImportRequest = components['schemas']['CreateProjectFromSourceRequest'];

	let { data }: { data: PageData } = $props();
	let canEdit = $derived(data.workspace?.role === 'owner' || data.workspace?.role === 'developer');
	const steppedIn = navigating.from?.url.pathname === '/projects/new';
	let serverId = $derived(page.url.searchParams.get('server_id') ?? '');
	let server = $derived<ServerResponse | null>(data.servers.find((s) => s.id === serverId) ?? null);
	let analysis = $state<AnalysisResponse | null>(null);
	let analyzing = $state(false);
	let submitting = $state(false);
	let error = $state('');
	let retrying = $state(false);
	let pending = $state<ImportRequest | null>(null);
	let completedProjectId = $state('');
	let pendingKey = $derived(`stagezero:source-import:${data.workspace?.id}:${serverId}`);

	onMount(() => {
		try {
			const saved = sessionStorage.getItem(pendingKey);
			if (saved) pending = JSON.parse(saved) as ImportRequest;
		} catch {
			// Browser storage can be unavailable; the server idempotency contract still works.
		}
	});

	async function retryLoadServers() {
		retrying = true;
		try {
			await invalidateAll();
		} finally {
			retrying = false;
		}
	}

	function goBack() {
		// eslint-disable-next-line svelte/no-navigation-without-resolve
		void goto('/projects/new');
	}

	function changeRepository() {
		analysis = null;
		error = '';
	}

	async function analyze(repoUrl: string, branch: string, branchConfirmed: boolean) {
		analyzing = true;
		error = '';
		try {
			const { data: result, error: err } = await api.POST('/api/source/analyze', {
				body: { repo_url: repoUrl, branch, branch_confirmed: branchConfirmed }
			});
			if (err || !result) {
				error = (err as { error?: string } | undefined)?.error ?? 'Could not analyze repository';
				return;
			}
			analysis = result;
		} catch {
			error = 'Network error';
		} finally {
			analyzing = false;
		}
	}

	async function create(values: {
		repoUrl: string;
		name: string;
		containerName: string;
		containerPort: number;
		branch: string;
		branchConfirmed: boolean;
	}) {
		if (submitting || !server || !analysis) return;
		const body: ImportRequest = {
			idempotency_key: crypto.randomUUID(),
			name: values.name,
			container_name: values.containerName,
			container_port: values.containerPort,
			server_id: server.id,
			repo_url: values.repoUrl,
			branch: values.branch,
			branch_confirmed: values.branchConfirmed
		};
		if (pending) {
			if (
				JSON.stringify({ ...pending, idempotency_key: '' }) !==
				JSON.stringify({ ...body, idempotency_key: '' })
			) {
				error =
					'Retry the pending import first, or choose Start another import to change the details.';
				return;
			}
			body.idempotency_key = pending.idempotency_key;
		}
		await submitImport(body);
	}

	async function submitImport(body: ImportRequest) {
		if (submitting) return;
		submitting = true;
		error = '';
		pending = body;
		try {
			sessionStorage.setItem(pendingKey, JSON.stringify(body));
		} catch {
			// A retry in this page still uses the same key even without storage.
		}
		try {
			const result = await api.POST('/api/projects/from-source', { body });
			if (result.error || !result.data) {
				const reason = (result.error as { error?: string } | undefined)?.error;
				error =
					reason ??
					(result.response.status === 409
						? 'This key belongs to a different import, or the container name is in use. Check the details before starting another import.'
						: result.response.status === 429
							? 'Too many analyses are running. Retry this request in a minute.'
							: result.response.status === 422
								? 'Repository analysis failed. Check the branch and project files, then retry.'
								: 'Could not complete the import. Retry this request to check its result.');
				return;
			}
			completedProjectId = result.data.project.id;
			pending = null;
			try {
				sessionStorage.removeItem(pendingKey);
			} catch {
				// Storage may be unavailable.
			}
			// eslint-disable-next-line svelte/no-navigation-without-resolve
			await goto(`/projects/${completedProjectId}`, {
				state: {
					toastFlash: {
						tone: 'success',
						title: 'Service created successfully',
						description: `Imported ${body.repo_url}.`
					}
				}
			});
		} catch {
			error = completedProjectId
				? 'Project created. Use the project link below if navigation did not finish.'
				: 'Connection lost. Retry with the same request to check the import result.';
		} finally {
			submitting = false;
		}
	}

	const pan = createCanvasPan({ bounds: 'auto' });
	const panViewport = pan.viewport;
</script>

<svelte:head>
	<title>Import a GitHub repository · StageZero</title>
</svelte:head>

<div
	class="canvas viewport relative flex w-full flex-1 overflow-hidden rounded-xl border border-border"
	data-panning={pan.isPanning ? 'true' : 'false'}
	use:panViewport
>
	<div
		class="canvas-bg"
		aria-hidden="true"
		style="background-size: {12 * pan.scale}px {12 * pan.scale}px; background-position: {pan.x -
			pan.scale}px {pan.y - pan.scale}px;"
	></div>
	<div class="scroll-area relative z-10 flex w-full overflow-x-hidden overflow-y-auto">
		<div
			class="stage m-auto flex min-h-full w-full items-center justify-center px-4 py-8 sm:px-6 sm:py-12"
			class:stage-enter={steppedIn}
		>
			<div
				class="world flex w-full items-center justify-center"
				style="transform: translate3d({pan.x}px, {pan.y}px, 0) scale({pan.scale});"
			>
				{#if !canEdit}
					<div class="w-full max-w-105" data-no-pan>
						<EmptyState
							icon={ServerStack}
							title="You don't have permission to create projects"
							description="Ask a workspace owner or developer to import a repository, or request a role change."
						>
							{#snippet actions()}<Button href="/projects" variant="secondary" size="sm"
									>Back to projects</Button
								>{/snippet}
						</EmptyState>
					</div>
				{:else if !serverId}
					<div class="w-full max-w-105" data-no-pan>
						<EmptyState
							icon={ServerStack}
							title="Pick a server first"
							description="Start by choosing the server this repository should deploy to."
						>
							{#snippet actions()}<Button href="/projects/new" variant="secondary" size="sm"
									><Icon src={ArrowLeft} theme="outline" class="h-3.5 w-3.5" />Back to starters</Button
								>{/snippet}
						</EmptyState>
					</div>
				{:else if data.serversError}
					<div class="w-full max-w-105" data-no-pan>
						<EmptyState
							icon={ServerStack}
							title="Couldn't load servers"
							description={data.serversError}
						>
							{#snippet actions()}<Button
									type="button"
									size="sm"
									loading={retrying}
									onclick={retryLoadServers}>Retry</Button
								><Button href="/projects/new" variant="secondary" size="sm"
									><Icon src={ArrowLeft} theme="outline" class="h-3.5 w-3.5" />Back to starters</Button
								>{/snippet}
						</EmptyState>
					</div>
				{:else if !server}
					<div class="w-full max-w-105" data-no-pan>
						<EmptyState
							icon={ServerStack}
							title="Server not found"
							description="That server is no longer available in this workspace. Pick a different one to continue."
						>
							{#snippet actions()}<Button href="/projects/new" variant="secondary" size="sm"
									><Icon src={ArrowLeft} theme="outline" class="h-3.5 w-3.5" />Back to starters</Button
								>{/snippet}
						</EmptyState>
					</div>
				{:else}
					<div class="w-full max-w-120" data-no-pan>
						{#if completedProjectId}
							<p class="mb-3 text-sm">
								<!-- eslint-disable-next-line svelte/no-navigation-without-resolve -->
								<a href={`/projects/${completedProjectId}`} class="underline"
									>Open created project</a
								>
							</p>
						{/if}
						{#if pending}
							<div class="mb-3 rounded-lg border border-border bg-card p-4 text-sm text-foreground">
								<p>
									Retry this request to recover a completed project without creating another. If the
									details need changing, start another import.
								</p>
								<div class="mt-3 flex flex-wrap gap-2">
									<Button
										type="button"
										size="sm"
										loading={submitting}
										onclick={() => pending && submitImport(pending)}>Retry import</Button
									>
									<Button
										type="button"
										variant="secondary"
										size="sm"
										onclick={() => {
											pending = null;
											try {
												sessionStorage.removeItem(pendingKey);
											} catch {
												// In-memory retry still works when storage is unavailable.
											}
										}}>Start another import</Button
									>
								</div>
							</div>
						{/if}
						<RepoStarterForm
							{analysis}
							{submitting}
							{analyzing}
							{error}
							onBack={goBack}
							onChangeRepository={changeRepository}
							onAnalyze={analyze}
							onCreate={create}
						>
							{#snippet details()}
								<div class="text-xs text-muted-foreground">
									<span>Deploys to </span><span class="font-medium text-foreground"
										>{server.name}</span
									>
									<span class="font-mono">({server.host}:{server.port})</span>
								</div>
							{/snippet}
						</RepoStarterForm>
					</div>
				{/if}
			</div>
		</div>
	</div>
	<div class="toolbar" data-no-pan aria-label="Canvas controls">
		<button
			type="button"
			class="tool-btn"
			onclick={() => pan.zoomOut()}
			disabled={pan.scale <= 0.8}
			aria-label="Zoom out"><Icon src={Minus} theme="outline" class="h-3.5 w-3.5" /></button
		>
		<button
			type="button"
			class="tool-btn zoom-label"
			onclick={() => pan.resetZoom()}
			aria-label="Reset zoom to 100%">{Math.round(pan.scale * 100)}%</button
		>
		<button
			type="button"
			class="tool-btn"
			onclick={() => pan.zoomIn()}
			disabled={pan.scale >= 1.4}
			aria-label="Zoom in"><Icon src={Plus} theme="outline" class="h-3.5 w-3.5" /></button
		>
		<span class="divider" aria-hidden="true"></span>
		<button
			type="button"
			class="tool-btn"
			onclick={() => pan.recenter()}
			aria-label="Recenter canvas"
			><Icon src={ArrowsPointingIn} theme="outline" class="h-3.5 w-3.5" /></button
		>
	</div>
</div>

<style>
	.canvas {
		background-color: var(--canvas);
	}
	@media (pointer: fine) {
		.viewport {
			cursor: grab;
		}
		.viewport[data-panning='true'] {
			cursor: grabbing;
			user-select: none;
		}
	}
	.canvas-bg {
		position: absolute;
		inset: 0;
		background-image: radial-gradient(circle at 1px 1px, var(--canvas-dot) 1px, transparent 0);
		background-size: 12px 12px;
		pointer-events: none;
	}
	.world {
		transform-origin: center center;
		will-change: transform;
	}
	.stage-enter {
		animation: stage-in-from-right 200ms cubic-bezier(0.23, 1, 0.32, 1) backwards;
	}
	@keyframes stage-in-from-right {
		from {
			opacity: 0;
			transform: translateX(12px);
		}
		to {
			opacity: 1;
			transform: translateX(0);
		}
	}
	.toolbar {
		display: none;
	}
	@media (pointer: fine) {
		.toolbar {
			position: absolute;
			bottom: 0.75rem;
			left: 0.75rem;
			z-index: 20;
			display: inline-flex;
			align-items: center;
			gap: 0.125rem;
			padding: 0.25rem;
			background: var(--card);
			border: 1px solid var(--border);
			border-radius: var(--radius-md);
			box-shadow: var(--shadow-panel);
			cursor: default;
		}
		.tool-btn {
			display: inline-flex;
			align-items: center;
			justify-content: center;
			min-width: 1.75rem;
			height: 1.75rem;
			padding: 0 0.375rem;
			border-radius: var(--radius-sm);
			font-size: 0.75rem;
			font-variant-numeric: tabular-nums;
			color: var(--muted-foreground);
			background: transparent;
			cursor: pointer;
			transition:
				background-color 120ms ease-out,
				color 120ms ease-out;
		}
		.tool-btn:hover:not(:disabled) {
			background: var(--accent);
			color: var(--accent-foreground);
		}
		.tool-btn:focus-visible {
			outline: none;
			box-shadow: 0 0 0 2px var(--ring);
			color: var(--foreground);
		}
		.tool-btn:disabled {
			opacity: 0.4;
			cursor: not-allowed;
		}
		.zoom-label {
			min-width: 2.5rem;
		}
		.divider {
			width: 1px;
			height: 1rem;
			margin: 0 0.125rem;
			background: var(--border);
		}
	}
</style>
