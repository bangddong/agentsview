// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { fireEvent, screen } from "@testing-library/svelte";
import { mount, tick, unmount } from "svelte";
import { router } from "../../stores/router.svelte.js";
import ProjectReclassificationModal from "./ProjectReclassificationModal.svelte";

const api = vi.hoisted(() => ({
  candidates: vi.fn(),
  preview: vi.fn(),
  apply: vi.fn(),
}));

vi.mock("../../api/generated/index", () => ({
  ActivityService: {
    getApiV1ActivityProjectReclassificationCandidates: api.candidates,
  },
  SettingsService: {
    postApiV1SettingsWorktreeMappingsPreview: api.preview,
    postApiV1SettingsWorktreeMappingsReclassify: api.apply,
  },
}));
vi.mock("../../api/runtime.js", () => ({
  callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
  isAbortError: vi.fn(() => false),
}));

const candidate = {
  id: "candidate-1",
  machine: "remote.example",
  suggested_prefix: "/srv/worktrees/example/repo/branch",
  contributing_sessions: 3,
  distinct_cwds: 2,
  evidence_kind: "identity",
  evidence_root: "/srv/worktrees/example/repo/branch",
  examples: [],
  available: true,
};

const preview = {
  mapping_token: "token-1",
  normalized_project: "target-project",
  matched_sessions: 7,
  updated_sessions: 6,
  distinct_projects: 2,
  project_samples: [
    { project: "wrong-project", count: 6 },
    { project: "another-project", count: 1 },
  ],
  session_samples: [],
};

async function flush() {
  await tick();
  await Promise.resolve();
  await tick();
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

describe("ProjectReclassificationModal", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    vi.useFakeTimers();
    api.candidates.mockReset();
    api.preview.mockReset();
    api.apply.mockReset();
    api.candidates.mockResolvedValue({ candidates: [candidate] });
    api.preview.mockResolvedValue(preview);
    api.apply.mockResolvedValue({ mapping: {}, result: preview });
  });

  afterEach(() => {
    if (component) unmount(component);
    component = undefined;
    document.body.innerHTML = "";
    vi.useRealTimers();
  });

  function render(overrides: Record<string, unknown> = {}) {
    component = mount(ProjectReclassificationModal, {
      target: document.body,
      props: {
        projectLabel: "wrong-project",
        projectKey: "pl1:sha256:wrong",
        projects: [{ name: "target-project", session_count: 12 }],
        queryParams: {
          preset: "custom",
          from: "2026-07-01T00:00:00Z",
          to: "2026-07-02T00:00:00Z",
          machine: "remote.example",
          automation: "all",
        },
        onclose: vi.fn(),
        onRefresh: vi.fn().mockResolvedValue(true),
        onComplete: vi.fn(),
        ...overrides,
      },
    });
  }

  async function chooseTarget() {
    await fireEvent.click(screen.getByTitle("Target project"));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "target-project (12)" }));
    await vi.advanceTimersByTimeAsync(300);
    await flush();
  }

  it("uses the clicked scope, preselects one candidate, and previews full-archive impact", async () => {
    render();
    await flush();

    expect(api.candidates).toHaveBeenCalledWith({
      preset: "custom",
      from: "2026-07-01T00:00:00Z",
      to: "2026-07-02T00:00:00Z",
      machine: "remote.example",
      automation: "all",
      clickedProject: "wrong-project",
      clickedProjectKey: "pl1:sha256:wrong",
    });
    expect(screen.getByText(/Originally shown as wrong-project/)).toBeTruthy();
    expect(screen.getByDisplayValue(candidate.suggested_prefix)).toBeTruthy();
    expect(screen.getByText("remote.example")).toBeTruthy();

    await chooseTarget();

    expect(api.preview).toHaveBeenCalledWith({
      requestBody: {
        machine: "remote.example",
        path_prefix: candidate.suggested_prefix,
        project: "target-project",
        original_project: "wrong-project",
        layout: "explicit",
        enabled: true,
      },
    });
    expect(screen.getByText(/7 sessions matched/)).toBeTruthy();
    expect(screen.getByText(/6 sessions will change/)).toBeTruthy();
    expect(screen.getByText(/2 projects/)).toBeTruthy();
    expect(screen.getByRole("alert").textContent).toContain("multiple projects");
    expect(screen.queryByText(/Will be saved as/)).toBeNull();
  });

  it("shows the server-normalized target when it differs from the typed one", async () => {
    api.preview.mockResolvedValueOnce({
      ...preview,
      normalized_project: "target_project",
    });
    render();
    await flush();
    await chooseTarget();

    expect(screen.getByText("Will be saved as target_project")).toBeTruthy();
  });

  it("debounces prefix edits and never accepts an obsolete preview token", async () => {
    render();
    await flush();
    await fireEvent.click(screen.getByTitle("Target project"));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "target-project (12)" }));
    const prefix = screen.getByLabelText("Path prefix");
    await fireEvent.input(prefix, { target: { value: "/srv/first" } });
    await vi.advanceTimersByTimeAsync(200);
    await fireEvent.input(prefix, { target: { value: "/srv/final" } });
    await vi.advanceTimersByTimeAsync(300);
    await flush();

    expect(api.preview).toHaveBeenCalledTimes(1);
    expect(api.preview.mock.calls[0]![0].requestBody.path_prefix).toBe("/srv/final");
  });

  it("commits a custom target and blocks a zero-match preview", async () => {
    api.preview.mockResolvedValueOnce({
      ...preview,
      mapping_token: "empty-token",
      matched_sessions: 0,
      updated_sessions: 0,
      distinct_projects: 0,
      project_samples: [],
    });
    render();
    await flush();
    await fireEvent.click(screen.getByTitle("Target project"));
    await fireEvent.input(screen.getByRole("combobox"), {
      target: { value: "new-project" },
    });
    await fireEvent.mouseDown(screen.getByRole("option", { name: 'Use project "new-project"' }));
    await vi.advanceTimersByTimeAsync(300);
    await flush();

    expect(api.preview.mock.calls[0]![0].requestBody.project).toBe("new-project");
    expect(screen.getByText(/matches no sessions/)).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Apply reclassification" }) as HTMLButtonElement,
    ).toHaveProperty("disabled", true);
  });

  it("keeps the selected target preview across repeated empty query resets", async () => {
    render();
    await flush();
    await fireEvent.click(screen.getByTitle("Target project"));
    await fireEvent.input(screen.getByRole("combobox"), {
      target: { value: "new-project" },
    });
    await fireEvent.mouseDown(
      screen.getByRole("option", { name: 'Use project "new-project"' }),
    );
    await flush();

    // Typeahead reports an empty query while closing after selection, then
    // again when it reopens and closes without a new selection. Those resets
    // do not change the selected target and must not cancel its preview.
    await fireEvent.click(screen.getByTitle("Target project"));
    await fireEvent.keyDown(screen.getByRole("combobox"), { key: "Escape" });
    await vi.advanceTimersByTimeAsync(300);
    await flush();

    expect(api.preview).toHaveBeenCalledTimes(1);
    expect(api.preview.mock.calls[0]![0].requestBody.project).toBe("new-project");
    expect(
      screen.getByRole("button", { name: "Apply reclassification" }) as HTMLButtonElement,
    ).toHaveProperty("disabled", false);
  });

  it("discards a preview response superseded by a later draft", async () => {
    let resolveFirst!: (value: typeof preview) => void;
    api.preview
      .mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            resolveFirst = resolve;
          }),
      )
      .mockResolvedValueOnce({ ...preview, mapping_token: "latest-token" });
    render();
    await flush();
    await chooseTarget();
    const prefix = screen.getByLabelText("Path prefix");
    await fireEvent.input(prefix, { target: { value: "/srv/latest" } });
    await vi.advanceTimersByTimeAsync(300);
    await flush();
    resolveFirst({ ...preview, mapping_token: "obsolete-token" });
    await flush();

    await fireEvent.click(screen.getByRole("button", { name: "Apply reclassification" }));
    await flush();
    expect(api.apply.mock.calls[0]![0].requestBody.mapping_token).toBe("latest-token");
  });

  it("invalidates an accepted preview as soon as the target query changes", async () => {
    render();
    await flush();
    await chooseTarget();
    expect(
      screen.getByRole("button", { name: "Apply reclassification" }) as HTMLButtonElement,
    ).toHaveProperty("disabled", false);

    await fireEvent.click(screen.getByTitle("Target project"));
    await fireEvent.input(screen.getByRole("combobox"), {
      target: { value: "target-pro" },
    });

    const apply = screen.getByRole("button", { name: "Apply reclassification" });
    expect(apply as HTMLButtonElement).toHaveProperty("disabled", true);
    await fireEvent.click(apply);
    expect(api.apply).not.toHaveBeenCalled();
  });

  it("stops showing a canceled preview as loading when the draft becomes invalid", async () => {
    const pending = deferred<typeof preview>();
    api.preview.mockReturnValueOnce(pending.promise);
    render();
    await flush();
    await fireEvent.click(screen.getByTitle("Target project"));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "target-project (12)" }));
    await vi.advanceTimersByTimeAsync(300);
    await flush();
    expect(screen.getByText(/Calculating full-archive impact/)).toBeTruthy();

    await fireEvent.input(screen.getByLabelText("Path prefix"), { target: { value: "" } });
    await flush();

    expect(screen.queryByText(/Calculating full-archive impact/)).toBeNull();
    expect(
      screen.getByRole("button", { name: "Apply reclassification" }) as HTMLButtonElement,
    ).toHaveProperty("disabled", true);
  });

  it("requires an explicit choice for multiple candidates and explains unavailable cwd evidence", async () => {
    api.candidates.mockResolvedValue({
      candidates: [
        candidate,
        {
          ...candidate,
          id: "candidate-2",
          machine: "other.example",
          suggested_prefix: "",
          available: false,
        },
      ],
    });
    render();
    await flush();

    expect(screen.getAllByText("Choose a worktree")).toHaveLength(2);
    expect(screen.queryByLabelText("Path prefix")).toBeNull();
    await fireEvent.click(screen.getByTitle("Choose a worktree"));
    await fireEvent.mouseDown(screen.getByRole("option", { name: /other\.example/ }));
    await flush();
    expect(screen.getByText(/working directory is unavailable/)).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Apply reclassification" }) as HTMLButtonElement,
    ).toHaveProperty("disabled", true);
  });

  it("applies exactly once and offers only refresh retry after a post-commit failure", async () => {
    const retry = deferred<boolean>();
    const onRefresh = vi.fn().mockResolvedValueOnce(false).mockReturnValueOnce(retry.promise);
    const onComplete = vi.fn();
    render({ onRefresh, onComplete });
    await flush();
    await chooseTarget();

    const apply = screen.getByRole("button", { name: "Apply reclassification" });
    await fireEvent.click(apply);
    await flush();

    expect(api.apply).toHaveBeenCalledTimes(1);
    expect(api.apply.mock.calls[0]![0].requestBody.mapping_token).toBe("token-1");
    expect(screen.queryByRole("button", { name: "Apply reclassification" })).toBeNull();
    expect(screen.getByText(/Applied, but Activity could not refresh/)).toBeTruthy();
    const retryButton = screen.getByRole("button", { name: "Retry refresh" });
    await fireEvent.click(retryButton);
    await fireEvent.click(retryButton);
    await flush();
    expect(api.apply).toHaveBeenCalledTimes(1);
    expect(onRefresh).toHaveBeenCalledTimes(2);
    expect(
      screen.getByRole("button", { name: "Refreshing…" }) as HTMLButtonElement,
    ).toHaveProperty("disabled", true);

    retry.resolve(true);
    await flush();
    expect(onComplete).toHaveBeenCalledTimes(1);
  });

  it("marks the initial post-commit refresh in flight before exposing a retry", async () => {
    const pendingRefresh = deferred<boolean>();
    const onRefresh = vi.fn().mockReturnValue(pendingRefresh.promise);
    render({ onRefresh });
    await flush();
    await chooseTarget();

    await fireEvent.click(screen.getByRole("button", { name: "Apply reclassification" }));
    await flush();

    const refreshing = screen.getByRole("button", { name: "Refreshing…" });
    expect(refreshing as HTMLButtonElement).toHaveProperty("disabled", true);
    expect(screen.queryByText(/Applied, but Activity could not refresh/)).toBeNull();
    await fireEvent.click(refreshing);
    expect(onRefresh).toHaveBeenCalledTimes(1);

    pendingRefresh.resolve(false);
    await flush();
    expect(screen.getByText(/Applied, but Activity could not refresh/)).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Retry refresh" }) as HTMLButtonElement,
    ).toHaveProperty("disabled", false);
  });

  it("suppresses every dismissal while apply is in flight and still refreshes after success", async () => {
    const pendingApply = deferred<{ mapping: object; result: typeof preview }>();
    const onclose = vi.fn();
    const onRefresh = vi.fn().mockResolvedValue(false);
    api.apply.mockReturnValueOnce(pendingApply.promise);
    render({ onclose, onRefresh });
    await flush();
    await chooseTarget();

    await fireEvent.click(screen.getByRole("button", { name: "Apply reclassification" }));
    await flush();
    const cancel = screen.getByRole("button", { name: "Cancel" });
    expect(cancel as HTMLButtonElement).toHaveProperty("disabled", true);
    await fireEvent.click(cancel);
    await fireEvent.click(screen.getByRole("button", { name: "Close project reclassification" }));
    await fireEvent.keyDown(window, { key: "Escape" });
    const overlay = document.querySelector(".kit-modal-overlay");
    expect(overlay).not.toBeNull();
    await fireEvent.pointerDown(overlay!);
    expect(onclose).not.toHaveBeenCalled();

    pendingApply.resolve({ mapping: {}, result: preview });
    await flush();
    expect(api.apply).toHaveBeenCalledTimes(1);
    expect(onRefresh).toHaveBeenCalledTimes(1);
  });

  it("localizes and number-formats project sample session counts", async () => {
    api.preview.mockResolvedValueOnce({
      ...preview,
      project_samples: [{ project: "wrong-project", count: 1234 }],
    });
    render();
    await flush();
    await chooseTarget();

    expect(screen.getByRole("alert").textContent).toContain(
      "wrong-project (1,234 sessions)",
    );
  });

  it("distinguishes same-machine worktrees in the collapsed candidate label", async () => {
    api.candidates.mockResolvedValue({
      candidates: [
        candidate,
        {
          ...candidate,
          id: "candidate-2",
          suggested_prefix: "/srv/worktrees/example/repo/other-branch",
        },
      ],
    });
    render();
    await flush();

    await fireEvent.click(screen.getByTitle("Choose a worktree"));
    await fireEvent.mouseDown(
      screen.getByRole("option", { name: /other-branch/ }),
    );
    await flush();

    expect(screen.getByTitle("Choose a worktree").textContent).toContain(
      "remote.example · repo/other-branch",
    );
  });

  it("deep-links Open Settings to the candidate's machine", async () => {
    const navigate = vi.spyOn(router, "navigate").mockReturnValue(true);
    const onclose = vi.fn();
    render({ onclose });
    await flush();

    await fireEvent.click(screen.getByRole("link", { name: "Open Settings" }));

    expect(onclose).toHaveBeenCalledTimes(1);
    expect(navigate).toHaveBeenCalledWith("settings", {
      worktree_machine: "remote.example",
    });
    navigate.mockRestore();
  });

  it("refreshes the preview after a mapping-set conflict", async () => {
    api.apply.mockRejectedValueOnce(Object.assign(new Error("changed"), { status: 409 }));
    render();
    await flush();
    await chooseTarget();
    await fireEvent.click(screen.getByRole("button", { name: "Apply reclassification" }));
    await flush();

    expect(api.apply).toHaveBeenCalledTimes(1);
    expect(api.preview).toHaveBeenCalledTimes(2);
    expect(screen.getByText(/changed since the preview/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Apply reclassification" })).toBeTruthy();
  });
});
