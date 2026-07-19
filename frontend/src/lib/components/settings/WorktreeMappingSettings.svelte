<script lang="ts">
  import { Button, Card, Checkbox, Modal, TextInput, Typeahead } from "@kenn-io/kit-ui";
  import { onDestroy } from "svelte";
  import {
    SettingsService,
    type ApplyWorktreeMappingsResponse,
    type DbWorktreeProjectMapping,
    type WorktreeMappingRequest,
    type WorktreeMappingsResponse,
  } from "../../api/generated/index";
  import { callGenerated, isAbortError } from "../../api/runtime.js";
  import { m } from "../../i18n/index.js";
  import { router } from "../../stores/router.svelte.js";
  import { LatestRead } from "../../utils/latest-read.js";
  import SettingsSection from "./SettingsSection.svelte";

  interface Props {
    readOnly?: boolean;
  }

  type Confirmation =
    | { kind: "delete"; mapping: DbWorktreeProjectMapping }
    | { kind: "disable"; id: number; input: WorktreeMappingRequest };

  const explicitLayout = "explicit";
  const repoDotWorktreesLayout = "repo_dot_worktrees";

  let { readOnly = false }: Props = $props();

  // Deep link from the Activity reclassification modal: preselect the rule's
  // machine on first load only.
  const requestedMachine = router.params["worktree_machine"] ?? "";

  let localMachine = $state("");
  let machine = $state("");
  let machines: string[] = $state([]);
  let mappings: DbWorktreeProjectMapping[] = $state([]);
  let loading = $state(true);
  let saving = $state(false);
  let applying = $state(false);
  let error = $state("");
  let applyMessage = $state("");
  let editingId: number | null = $state(null);
  let editingWasEnabled = $state(false);
  let pathPrefix = $state("");
  let layout = $state(explicitLayout);
  let project = $state("");
  let enabled = $state(true);
  let confirmation: Confirmation | null = $state(null);
  const mappingsRead = new LatestRead();
  let machineGeneration = 0;
  let disposed = false;

  const machineOptions = $derived(
    machines.map((name) => ({ name, label: name, displayLabel: name })),
  );
  const isRepoDotWorktrees = $derived(layout === repoDotWorktreesLayout);
  const canSave = $derived(
    pathPrefix.trim() !== "" &&
      (layout === repoDotWorktreesLayout || project.trim() !== ""),
  );

  $effect(() => {
    if (readOnly) {
      mappingsRead.cancel();
      loading = false;
      return;
    }
    void loadMappings(requestedMachine || undefined);
  });

  async function loadMappings(requestedMachine?: string) {
    if (disposed) return;
    const signal = mappingsRead.begin();
    loading = true;
    error = "";
    try {
      const res = await callGenerated(
        () =>
          SettingsService.getApiV1SettingsWorktreeMappings({
            machine: requestedMachine || undefined,
          }),
        signal,
      ) as WorktreeMappingsResponse;
      if (!mappingsRead.isCurrent(signal)) return;
      localMachine = res.local_machine;
      machine = res.machine;
      machines = Array.from(new Set([res.machine, ...((res.machines ?? []) as string[])]))
        .filter(Boolean);
      mappings = (res.mappings ?? []) as DbWorktreeProjectMapping[];
    } catch (err) {
      if (isAbortError(err) || !mappingsRead.isCurrent(signal)) return;
      error = err instanceof Error ? err.message : m.worktree_failed_load();
    } finally {
      if (mappingsRead.finish(signal)) loading = false;
    }
  }

  onDestroy(() => {
    disposed = true;
    mappingsRead.cancel();
  });

  function resetForm() {
    editingId = null;
    editingWasEnabled = false;
    pathPrefix = "";
    layout = explicitLayout;
    project = "";
    enabled = true;
    confirmation = null;
  }

  function selectMachine(value: string) {
    if (!value || value === machine) return;
    machineGeneration += 1;
    machine = value;
    mappings = [];
    saving = false;
    applying = false;
    applyMessage = "";
    error = "";
    resetForm();
    void loadMappings(value);
  }

  function isCurrentMachine(initiatingMachine: string, generation: number) {
    return !disposed && machine === initiatingMachine && machineGeneration === generation;
  }

  function editMapping(mapping: DbWorktreeProjectMapping) {
    editingId = mapping.id;
    editingWasEnabled = mapping.enabled;
    pathPrefix = mapping.path_prefix;
    layout = mapping.layout || explicitLayout;
    project = mapping.project;
    enabled = mapping.enabled;
    applyMessage = "";
    error = "";
  }

  function mappingInput(): WorktreeMappingRequest | null {
    const input = {
      machine,
      path_prefix: pathPrefix.trim(),
      layout,
      project: project.trim(),
      enabled,
    } satisfies WorktreeMappingRequest;
    if (!input.path_prefix) return null;
    if (layout !== repoDotWorktreesLayout && !input.project) return null;
    return input;
  }

  async function requestSave() {
    const input = mappingInput();
    if (!input) return;
    if (editingId != null && editingWasEnabled && !input.enabled) {
      confirmation = { kind: "disable", id: editingId, input };
      return;
    }
    await saveMapping(input, editingId);
  }

  async function saveMapping(input: WorktreeMappingRequest, id: number | null) {
    const initiatingMachine = input.machine ?? machine;
    const generation = machineGeneration;
    saving = true;
    error = "";
    applyMessage = "";
    try {
      if (id == null) {
        await callGenerated(() =>
          SettingsService.postApiV1SettingsWorktreeMappings({ requestBody: input }),
        );
      } else {
        await callGenerated(() =>
          SettingsService.putApiV1SettingsWorktreeMappingsId({
            id: String(id),
            requestBody: input,
          }),
        );
      }
      if (!isCurrentMachine(initiatingMachine, generation)) return;
      resetForm();
      await loadMappings(initiatingMachine);
    } catch (err) {
      if (!isCurrentMachine(initiatingMachine, generation)) return;
      error = err instanceof Error ? err.message : m.worktree_failed_save();
    } finally {
      if (isCurrentMachine(initiatingMachine, generation)) saving = false;
    }
  }

  function requestDelete(mapping: DbWorktreeProjectMapping) {
    confirmation = { kind: "delete", mapping };
  }

  async function removeMapping(mapping: DbWorktreeProjectMapping) {
    const initiatingMachine = machine;
    const generation = machineGeneration;
    saving = true;
    error = "";
    applyMessage = "";
    try {
      await callGenerated(() =>
        SettingsService.deleteApiV1SettingsWorktreeMappingsId({
          id: String(mapping.id),
        }),
      );
      if (!isCurrentMachine(initiatingMachine, generation)) return;
      if (editingId === mapping.id) resetForm();
      await loadMappings(initiatingMachine);
    } catch (err) {
      if (!isCurrentMachine(initiatingMachine, generation)) return;
      error = err instanceof Error ? err.message : m.worktree_failed_delete();
    } finally {
      if (isCurrentMachine(initiatingMachine, generation)) saving = false;
    }
  }

  async function confirmChange() {
    const pending = confirmation;
    confirmation = null;
    if (!pending) return;
    if (pending.kind === "delete") {
      await removeMapping(pending.mapping);
    } else {
      await saveMapping(pending.input, pending.id);
    }
  }

  async function applyMappings() {
    const initiatingMachine = machine;
    const generation = machineGeneration;
    applying = true;
    error = "";
    applyMessage = "";
    try {
      const res = await callGenerated(() =>
        SettingsService.postApiV1SettingsWorktreeMappingsApply({
          requestBody: { machine: initiatingMachine },
        }),
      ) as ApplyWorktreeMappingsResponse;
      if (!isCurrentMachine(initiatingMachine, generation)) return;
      applyMessage = m.worktree_apply_result({
        updated: res.updated_sessions,
        matched: res.matched_sessions,
      });
    } catch (err) {
      if (!isCurrentMachine(initiatingMachine, generation)) return;
      error = err instanceof Error ? err.message : m.worktree_failed_apply();
    } finally {
      if (isCurrentMachine(initiatingMachine, generation)) applying = false;
    }
  }
</script>

{#snippet confirmationActions()}
  <Button
    label={m.worktree_cancel()}
    tone="neutral"
    surface="outline"
    disabled={saving}
    onclick={() => (confirmation = null)}
  />
  <Button
    label={confirmation?.kind === "delete"
      ? m.worktree_delete_confirm_action()
      : m.worktree_disable_confirm_action()}
    tone="danger"
    surface="solid"
    disabled={saving}
    onclick={confirmChange}
  />
{/snippet}

<SettingsSection title={m.worktree_title()} description={m.worktree_description()}>
  {#if readOnly}
    <div class="muted">{m.worktree_local_only()}</div>
  {:else if loading && machine === ""}
    <div class="muted">{m.worktree_loading()}</div>
  {:else if error && machine === ""}
    <div class="error-text">{error}</div>
  {:else}
    <div class="machine-row">
      <span class="label">{m.worktree_machine()}</span>
      <Typeahead
        options={machineOptions}
        value={machine}
        fallbackLabel={machine || localMachine}
        placeholder={m.worktree_select_machine()}
        title={m.worktree_select_machine()}
        emptyLabel={m.worktree_no_machines()}
        onselect={selectMachine}
      />
    </div>

    <div class="mapping-list" aria-busy={loading}>
      {#if loading}
        <div class="muted">{m.worktree_loading()}</div>
      {:else if mappings.length === 0}
        <div class="empty">{m.worktree_no_mappings()}</div>
      {:else}
        {#each mappings as mapping (mapping.id)}
          <Card level="inset" padding="sm">
            <div class="mapping-row-content" class:disabled={!mapping.enabled}>
              <div class="mapping-main">
                <div class="mapping-project">
                  {mapping.project ||
                    (mapping.layout === repoDotWorktreesLayout
                      ? m.worktree_layout_repo_dot_worktrees({ repo: "repo", branch: "branch" })
                      : m.worktree_layout_explicit({}))}
                </div>
                <div class="mapping-path">{mapping.path_prefix}</div>
                {#if mapping.original_project}
                  <div class="mapping-original">
                    {m.worktree_originally_shown_as({ project: mapping.original_project })}
                  </div>
                {/if}
              </div>
              <div class="mapping-actions">
                <span class="status">{mapping.enabled ? m.worktree_on() : m.worktree_off()}</span>
                <Button size="sm" label={m.worktree_edit()} onclick={() => editMapping(mapping)} />
                <Button
                  size="sm"
                  tone="danger"
                  label={m.worktree_delete()}
                  onclick={() => requestDelete(mapping)}
                />
              </div>
            </div>
          </Card>
        {/each}
      {/if}
    </div>

    <div class="form-grid">
      <div class="field">
        <span>{m.worktree_layout()}</span>
        <div class="layout-options" role="group" aria-label={m.worktree_layout()}>
          <Button
            label={m.worktree_layout_explicit({})}
            tone={layout === explicitLayout ? "info" : "neutral"}
            surface={layout === explicitLayout ? "soft" : "outline"}
            onclick={() => (layout = explicitLayout)}
          />
          <Button
            label={m.worktree_layout_repo_dot_worktrees({ repo: "repo", branch: "branch" })}
            tone={layout === repoDotWorktreesLayout ? "info" : "neutral"}
            surface={layout === repoDotWorktreesLayout ? "soft" : "outline"}
            onclick={() => (layout = repoDotWorktreesLayout)}
          />
        </div>
      </div>
      <label class="field">
        <span>{isRepoDotWorktrees ? m.worktree_parent_directory() : m.worktree_path_prefix()}</span>
        <TextInput
          bind:value={pathPrefix}
          block
          ariaLabel={isRepoDotWorktrees ? m.worktree_parent_directory() : m.worktree_path_prefix()}
          placeholder={isRepoDotWorktrees ? "/Users/me" : "/Users/me/project.worktrees"}
        />
        {#if isRepoDotWorktrees}
          <div class="hint">{m.worktree_parent_directory_hint()}</div>
        {/if}
      </label>
      <label class="field">
        <span>{m.worktree_project()}</span>
        <TextInput
          bind:value={project}
          block
          ariaLabel={m.worktree_project()}
          placeholder="project-name"
          disabled={isRepoDotWorktrees}
        />
        <div class="hint">
          {isRepoDotWorktrees ? m.worktree_project_derived() : m.worktree_project_required()}
        </div>
      </label>
      <Checkbox bind:checked={enabled} label={m.worktree_enabled()} />
    </div>

    {#if error}
      <div class="error-text">{error}</div>
    {/if}
    {#if applyMessage}
      <div class="success-text">{applyMessage}</div>
    {/if}

    <div class="button-row">
      <Button
        label={saving
          ? m.worktree_saving()
          : editingId == null
            ? m.worktree_add_mapping()
            : m.worktree_save_mapping()}
        tone="info"
        surface="solid"
        disabled={!canSave || saving}
        onclick={requestSave}
      />
      {#if editingId != null}
        <Button label={m.worktree_cancel()} disabled={saving} onclick={resetForm} />
      {/if}
      <Button
        label={applying ? m.worktree_applying() : m.worktree_apply_mappings()}
        disabled={applying || loading || mappings.length === 0}
        onclick={applyMappings}
      />
    </div>
  {/if}
</SettingsSection>

{#if confirmation}
  <Modal
    title={confirmation.kind === "delete"
      ? m.worktree_delete_confirm_title()
      : m.worktree_disable_confirm_title()}
    closeLabel={m.worktree_confirmation_close()}
    tone="danger"
    width="440px"
    onclose={() => (confirmation = null)}
    footer={confirmationActions}
  >
    <p class="confirmation-copy">{m.worktree_change_warning()}</p>
  </Modal>
{/if}

<style>
  .machine-row,
  .button-row,
  .mapping-actions {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .machine-row {
    justify-content: space-between;
    font-size: 12px;
  }

  .label,
  .field > span {
    color: var(--text-secondary);
    font-size: 12px;
    font-weight: 500;
  }

  .mapping-list {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }

  .mapping-row-content {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }

  .mapping-row-content.disabled {
    opacity: 0.65;
  }

  .mapping-main {
    min-width: 0;
  }

  .mapping-project {
    color: var(--text-primary);
    font-size: 12px;
    font-weight: 600;
  }

  .mapping-path {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--text-muted);
    font-family: var(--font-mono, monospace);
    font-size: 11px;
  }

  .mapping-original,
  .status,
  .hint,
  .muted,
  .empty {
    color: var(--text-muted);
    font-size: 11px;
  }

  .mapping-original {
    margin-top: 2px;
  }

  .form-grid {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: var(--space-5);
  }

  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    min-width: 0;
  }

  /* Stacked: the long repo_dot_worktrees label does not fit two-up inside a
     one-third-width form column. */
  .layout-options {
    display: flex;
    flex-direction: column;
    align-items: stretch;
    gap: 4px;
  }

  .error-text,
  .success-text {
    font-size: 12px;
  }

  .error-text {
    color: var(--accent-red);
  }

  .success-text {
    color: var(--accent-green);
  }

  .confirmation-copy {
    margin: 0;
    color: var(--text-secondary);
    font-size: 13px;
    line-height: 1.5;
  }

  @media (max-width: 760px) {
    .form-grid {
      grid-template-columns: 1fr;
    }

    .mapping-row-content {
      align-items: flex-start;
      flex-direction: column;
    }
  }
</style>
