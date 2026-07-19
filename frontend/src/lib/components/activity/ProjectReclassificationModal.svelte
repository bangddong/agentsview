<script lang="ts">
  import { Button, Modal, TextInput, Typeahead, type TypeaheadOption } from "@kenn-io/kit-ui";
  import { onDestroy, onMount } from "svelte";
  import {
    ActivityService,
    SettingsService,
    type DbWorktreeReclassificationCandidate,
    type DbWorktreeReclassificationPreview,
  } from "../../api/generated/index";
  import { callGenerated, isAbortError } from "../../api/runtime.js";
  import { m } from "../../i18n/index.js";
  import type { ProjectInfo } from "../../api/types/core.js";
  import type { ActivityQueryParams } from "../../stores/activity.svelte.js";
  import { getBasePath, router } from "../../stores/router.svelte.js";
  import { LatestRead } from "../../utils/latest-read.js";
  import ProjectTypeahead from "../layout/ProjectTypeahead.svelte";

  interface Props {
    projectLabel: string;
    projectKey: string;
    projects: ProjectInfo[];
    queryParams: ActivityQueryParams;
    onclose: () => void;
    onRefresh: () => Promise<boolean>;
    onComplete: () => void;
  }

  let {
    projectLabel,
    projectKey,
    projects,
    queryParams,
    onclose,
    onRefresh,
    onComplete,
  }: Props = $props();

  let candidates = $state<DbWorktreeReclassificationCandidate[]>([]);
  let candidatesLoading = $state(true);
  let candidatesError = $state("");
  let selectedCandidateId = $state("");
  let machine = $state("");
  let pathPrefix = $state("");
  let targetProject = $state("");
  let preview = $state<DbWorktreeReclassificationPreview | null>(null);
  let previewLoading = $state(false);
  let previewError = $state("");
  let conflict = $state(false);
  let applying = $state(false);
  let applied = $state(false);
  let refreshing = $state(false);
  let applyError = $state("");
  let previewTimer: ReturnType<typeof setTimeout> | undefined;
  let disposed = false;
  const candidatesRead = new LatestRead();
  const previewRead = new LatestRead();

  const selectedCandidate = $derived(
    candidates.find((candidate) => candidate.id === selectedCandidateId),
  );
  const candidateOptions = $derived.by((): TypeaheadOption[] =>
    candidates.map((candidate) => ({
      name: candidate.id,
      label: `${candidate.machine} · ${candidate.suggested_prefix || m.activity_reclassify_path_unavailable()} · ${m.activity_reclassify_candidate_sessions({ count: candidate.contributing_sessions })}`,
      displayLabel: candidateDisplayLabel(candidate),
    })),
  );

  // The collapsed control shows only this label, so machine name alone cannot
  // distinguish two worktrees on the same machine.
  function candidateDisplayLabel(
    candidate: DbWorktreeReclassificationCandidate,
  ): string {
    const tail = candidate.suggested_prefix
      .split("/")
      .filter(Boolean)
      .slice(-2)
      .join("/");
    return tail ? `${candidate.machine} · ${tail}` : candidate.machine;
  }
  const canApply = $derived(
    !applied &&
      !applying &&
      !previewLoading &&
      !!preview?.mapping_token &&
      preview.matched_sessions > 0,
  );
  const settingsHref = $derived(
    machine
      ? `${getBasePath()}/settings?worktree_machine=${encodeURIComponent(machine)}`
      : `${getBasePath()}/settings`,
  );

  onMount(() => void loadCandidates());
  onDestroy(() => {
    disposed = true;
    if (previewTimer !== undefined) clearTimeout(previewTimer);
    candidatesRead.cancel();
    previewRead.cancel();
  });

  async function loadCandidates() {
    const signal = candidatesRead.begin();
    candidatesLoading = true;
    candidatesError = "";
    try {
      const response = await callGenerated(
        () => ActivityService.getApiV1ActivityProjectReclassificationCandidates({
          ...queryParams,
          clickedProject: projectLabel,
          clickedProjectKey: projectKey,
        }),
        signal,
      );
      if (!candidatesRead.isCurrent(signal)) return;
      candidates = (response.candidates ?? []) as DbWorktreeReclassificationCandidate[];
      if (candidates.length === 1) selectCandidate(candidates[0]!.id);
    } catch (error) {
      if (isAbortError(error) || !candidatesRead.isCurrent(signal)) return;
      candidatesError = error instanceof Error
        ? error.message
        : m.activity_reclassify_candidates_failed();
    } finally {
      if (candidatesRead.finish(signal)) candidatesLoading = false;
    }
  }

  function clearAcceptedPreview() {
    previewRead.cancel();
    previewLoading = false;
    preview = null;
    previewError = "";
    conflict = false;
    if (previewTimer !== undefined) clearTimeout(previewTimer);
    previewTimer = undefined;
  }

  function selectCandidate(id: string) {
    clearAcceptedPreview();
    selectedCandidateId = id;
    const candidate = candidates.find((item) => item.id === id);
    machine = candidate?.machine ?? "";
    pathPrefix = candidate?.suggested_prefix ?? "";
    schedulePreview();
  }

  function editPrefix(value: string) {
    pathPrefix = value;
    clearAcceptedPreview();
    schedulePreview();
  }

  function selectTarget(value: string) {
    targetProject = value.trim();
    clearAcceptedPreview();
    schedulePreview();
  }

  function editTargetQuery(value: string) {
    // Typeahead reports an empty query whenever it opens or closes, and a real
    // browser can report the close reset more than once during focus handoff.
    // That does not change the selected target. Non-empty edits still make an
    // accepted preview stale immediately; selecting a value clears and
    // reschedules the preview in selectTarget above.
    if (value === "") return;
    clearAcceptedPreview();
  }

  function draft() {
    return {
      machine,
      path_prefix: pathPrefix.trim(),
      project: targetProject.trim(),
      original_project: projectLabel,
      layout: "explicit",
      enabled: true,
    };
  }

  function schedulePreview(delay = 300) {
    if (previewTimer !== undefined) clearTimeout(previewTimer);
    previewTimer = undefined;
    if (!selectedCandidate?.available || !machine || !pathPrefix.trim() || !targetProject.trim()) {
      return;
    }
    previewTimer = setTimeout(() => void loadPreview(), delay);
  }

  async function loadPreview() {
    previewTimer = undefined;
    const requestBody = draft();
    if (!requestBody.machine || !requestBody.path_prefix || !requestBody.project) return;
    const signal = previewRead.begin();
    previewLoading = true;
    previewError = "";
    try {
      const result = await callGenerated(
        () => SettingsService.postApiV1SettingsWorktreeMappingsPreview({ requestBody }),
        signal,
      );
      if (!previewRead.isCurrent(signal)) return;
      preview = result;
    } catch (error) {
      if (isAbortError(error) || !previewRead.isCurrent(signal)) return;
      preview = null;
      previewError = error instanceof Error
        ? error.message
        : m.activity_reclassify_preview_failed();
    } finally {
      if (previewRead.finish(signal)) previewLoading = false;
    }
  }

  async function apply() {
    const token = preview?.mapping_token;
    if (!canApply || !token) return;
    applying = true;
    applyError = "";
    try {
      await callGenerated(() =>
        SettingsService.postApiV1SettingsWorktreeMappingsReclassify({
          requestBody: { ...draft(), mapping_token: token },
        }),
      );
      if (disposed) return;
      applied = true;
      // Keep dismissal blocked through the initial refresh as well as the
      // mutation request. The refresh has already started after commit, and
      // the modal remains present until it can show either completion or the
      // refresh-only retry state.
      await refreshActivity();
    } catch (error) {
      if (disposed) return;
      if (typeof error === "object" && error !== null && "status" in error && error.status === 409) {
        clearAcceptedPreview();
        conflict = true;
        await loadPreview();
      } else {
        applyError = error instanceof Error
          ? error.message
          : m.activity_reclassify_apply_failed();
      }
    } finally {
      if (!disposed) applying = false;
    }
  }

  async function retryRefresh() {
    if (!applied || refreshing) return;
    await refreshActivity();
  }

  async function refreshActivity() {
    refreshing = true;
    let refreshed = false;
    try {
      refreshed = await onRefresh();
    } catch {
      // The mutation has committed; a refresh error must stay on the
      // refresh-only path rather than being misreported as an apply failure.
    }
    if (disposed) return;
    refreshing = false;
    if (refreshed) onComplete();
  }

  function requestClose() {
    if (applying) return;
    onclose();
  }

  function openSettings(event: MouseEvent) {
    event.preventDefault();
    if (applying) return;
    onclose();
    router.navigate("settings", machine ? { worktree_machine: machine } : {});
  }

</script>

{#snippet footer()}
  {#if applied}
    <Button
      label={refreshing
        ? m.activity_reclassify_refreshing()
        : m.activity_reclassify_retry_refresh()}
      disabled={refreshing}
      tone="info"
      surface="solid"
      onclick={retryRefresh}
    />
  {:else}
    <Button label={m.activity_reclassify_cancel()} disabled={applying} onclick={requestClose} />
    <Button
      label={applying
        ? m.activity_reclassify_applying()
        : m.activity_reclassify_apply()}
      disabled={!canApply}
      tone="info"
      surface="solid"
      onclick={apply}
    />
  {/if}
{/snippet}

<Modal
  title={m.activity_reclassify_title()}
  closeLabel={m.activity_reclassify_close()}
  width="560px"
  maxWidth="min(560px, calc(100vw - 32px))"
  onclose={requestClose}
  footer={footer}
>
  <div class="modal-content">
    <p class="original">{m.activity_reclassify_original({ project: projectLabel })}</p>

    {#if candidatesLoading}
      <p class="muted">{m.activity_reclassify_candidates_loading()}</p>
    {:else if candidatesError}
      <p class="error-text">{candidatesError}</p>
    {:else if candidates.length === 0}
      <p class="muted">{m.activity_reclassify_no_candidates()}</p>
    {:else}
      {#if candidates.length > 1}
        <label class="field">
          <span>{m.activity_reclassify_choose_worktree()}</span>
          <Typeahead
            options={candidateOptions}
            value={selectedCandidateId}
            fallbackLabel={m.activity_reclassify_choose_worktree()}
            placeholder={m.activity_reclassify_choose_worktree()}
            title={m.activity_reclassify_choose_worktree()}
            emptyLabel={m.activity_reclassify_no_candidates()}
            onselect={selectCandidate}
          />
        </label>
      {/if}

      {#if selectedCandidate}
        <div class="candidate-summary">
          <strong>{selectedCandidate.machine}</strong>
          <span>{m.activity_reclassify_candidate_sessions({ count: selectedCandidate.contributing_sessions })}</span>
        </div>
        {#if !selectedCandidate.available}
          <p class="warning" role="alert">
            {m.activity_reclassify_cwd_unavailable()}
          </p>
        {:else}
          <label class="field">
            <span>{m.activity_reclassify_path_prefix()}</span>
            <TextInput
              value={pathPrefix}
              block
              ariaLabel={m.activity_reclassify_path_prefix()}
              oninput={editPrefix}
            />
          </label>
          <label class="field target-field">
            <span>{m.activity_reclassify_target_project()}</span>
            <ProjectTypeahead
              {projects}
              value={targetProject}
              onselect={selectTarget}
              onquery={editTargetQuery}
              includeAll={false}
              allowCustom={true}
              customLabel={m.activity_reclassify_use_custom_project({ query: "{query}" })}
              placeholder={m.activity_reclassify_target_project()}
              title={m.activity_reclassify_target_project()}
            />
          </label>
        {/if}
      {/if}
    {/if}

    {#if previewLoading}
      <p class="muted">{m.activity_reclassify_previewing()}</p>
    {:else if preview}
      <strong class="impact-title">{m.activity_reclassify_full_archive_impact()}</strong>
      <div class="impact" aria-live="polite">
        <span>{m.activity_reclassify_sessions_matched({ count: preview.matched_sessions })}</span>
        <span>{m.activity_reclassify_sessions_changing({ count: preview.updated_sessions })}</span>
        <span>{m.activity_reclassify_projects_affected({ count: preview.distinct_projects })}</span>
      </div>
      {#if preview.normalized_project && preview.normalized_project !== targetProject.trim()}
        <p class="normalized">
          {m.activity_reclassify_normalized_target({ project: preview.normalized_project })}
        </p>
      {/if}
      {#if preview.distinct_projects > 1}
        <div class="warning" role="alert">
          {m.activity_reclassify_multiple_projects()}
          {#if preview.project_samples?.length}
            <ul>
              {#each preview.project_samples as sample}
                <li>{sample.project} ({m.activity_reclassify_project_sample_sessions({ count: sample.count })})</li>
              {/each}
            </ul>
          {/if}
        </div>
      {:else if preview.matched_sessions === 0}
        <p class="error-text">{m.activity_reclassify_zero_matches()}</p>
      {/if}
    {/if}

    {#if conflict}
      <p class="warning">{m.activity_reclassify_conflict()}</p>
    {/if}
    {#if previewError}<p class="error-text">{previewError}</p>{/if}
    {#if applyError}<p class="error-text">{applyError}</p>{/if}
    {#if applied && !refreshing}
      <p class="warning" role="status">{m.activity_reclassify_applied_refresh_failed()}</p>
    {/if}

    <p class="settings-note">
      {m.activity_reclassify_managed_in_settings()}
      <a href={settingsHref} onclick={openSettings}>{m.activity_reclassify_open_settings()}</a>
    </p>
  </div>
</Modal>

<style>
  .modal-content,
  .field {
    display: flex;
    flex-direction: column;
  }

  .modal-content { gap: 12px; }
  .field { gap: var(--space-2); font-size: 12px; }
  .impact-title { font-size: 12px; }
  .target-field { --typeahead-min-width: 100%; }
  .original { margin: 0; color: var(--text-secondary); }
  .muted, .settings-note { color: var(--text-muted); font-size: 11px; }
  .candidate-summary, .impact { display: flex; gap: 12px; font-size: 12px; }
  .candidate-summary { justify-content: space-between; }
  .impact { flex-wrap: wrap; padding: 8px; background: var(--bg-inset); border-radius: var(--radius-sm); }
  .normalized { color: var(--text-secondary); font-size: 12px; }
  .warning { color: var(--accent-orange); font-size: 12px; }
  .error-text { color: var(--accent-red); font-size: 12px; }
  .warning, .error-text, .muted, .normalized, .settings-note { margin: 0; }
  .warning ul { margin: 6px 0 0; padding-left: 18px; }
  .settings-note a { margin-left: 4px; color: var(--accent-blue); }
</style>
