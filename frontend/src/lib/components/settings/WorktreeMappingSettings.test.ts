// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { fireEvent, screen } from "@testing-library/svelte";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import WorktreeMappingSettings from "./WorktreeMappingSettings.svelte";
import { SettingsService } from "../../api/generated/index";
import { router } from "../../stores/router.svelte.js";

vi.mock("../../api/runtime.js", async (importOriginal) => {
  const orig =
    await importOriginal<typeof import("../../api/runtime.js")>();
  return {
    ...orig,
    callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
  };
});

vi.mock("../../api/generated/index", async (importOriginal) => {
  const orig =
    await importOriginal<typeof import("../../api/generated/index")>();
  return {
    ...orig,
    SettingsService: {
      getApiV1SettingsWorktreeMappings: vi.fn(),
      postApiV1SettingsWorktreeMappings: vi.fn(),
      putApiV1SettingsWorktreeMappingsId: vi.fn(),
      deleteApiV1SettingsWorktreeMappingsId: vi.fn(),
      postApiV1SettingsWorktreeMappingsApply: vi.fn(),
    },
  };
});

const settingsService = SettingsService as unknown as {
  getApiV1SettingsWorktreeMappings: ReturnType<typeof vi.fn>;
  postApiV1SettingsWorktreeMappings: ReturnType<typeof vi.fn>;
  putApiV1SettingsWorktreeMappingsId: ReturnType<typeof vi.fn>;
  deleteApiV1SettingsWorktreeMappingsId: ReturnType<typeof vi.fn>;
  postApiV1SettingsWorktreeMappingsApply: ReturnType<typeof vi.fn>;
};

function response(machine: string, mappings: unknown[] = []) {
  return {
    local_machine: "local-host",
    machine,
    machines: ["local-host", "remote-host"],
    mappings,
  };
}

function mapping(overrides: Record<string, unknown> = {}) {
  return {
    id: 1,
    machine: "remote-host",
    path_prefix: "/srv/worktrees/example",
    layout: "explicit",
    project: "canonical-project",
    original_project: "branch-label",
    enabled: true,
    created_at: "2026-07-04T00:00:00.000Z",
    updated_at: "2026-07-04T00:00:00.000Z",
    ...overrides,
  };
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

async function flush() {
  await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();
}

describe("WorktreeMappingSettings", () => {
  let component: Record<string, unknown> | null = null;

  beforeEach(() => {
    vi.clearAllMocks();
    settingsService.getApiV1SettingsWorktreeMappings.mockReset();
    settingsService.postApiV1SettingsWorktreeMappings.mockReset();
    settingsService.putApiV1SettingsWorktreeMappingsId.mockReset();
    settingsService.deleteApiV1SettingsWorktreeMappingsId.mockReset();
    settingsService.postApiV1SettingsWorktreeMappingsApply.mockReset();
  });

  afterEach(() => {
    if (component) unmount(component);
    component = null;
    document.body.innerHTML = "";
  });

  it("does not request local worktree mappings in read-only mode", () => {
    component = mount(WorktreeMappingSettings, {
      target: document.body,
      props: {
        readOnly: true,
      },
    });

    expect(
      settingsService.getApiV1SettingsWorktreeMappings,
    ).not.toHaveBeenCalled();
    expect(document.body.textContent).toContain("local mode");

  });

  it("preselects the machine named by the worktree_machine deep link", async () => {
    router.params = { worktree_machine: "remote-host" };
    settingsService.getApiV1SettingsWorktreeMappings.mockResolvedValue(
      response("remote-host", [mapping()]),
    );

    component = mount(WorktreeMappingSettings, { target: document.body });
    await flush();

    expect(settingsService.getApiV1SettingsWorktreeMappings).toHaveBeenCalledWith(
      { machine: "remote-host" },
    );
    expect(document.body.textContent).toContain("canonical-project");
    router.params = {};
  });

  it("ignores a stale machine response after the selection changes", async () => {
    let resolveRemote!: (value: unknown) => void;
    let resolveLocal!: (value: unknown) => void;
    settingsService.getApiV1SettingsWorktreeMappings
      .mockResolvedValueOnce(response("local-host", [mapping({ machine: "local-host", project: "local-project" })]))
      .mockReturnValueOnce(new Promise((resolve) => (resolveRemote = resolve)))
      .mockReturnValueOnce(new Promise((resolve) => (resolveLocal = resolve)));

    component = mount(WorktreeMappingSettings, { target: document.body });
    await flush();

    await fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    expect(screen.getByRole("button", { name: "Save mapping" })).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Select machine" }));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "remote-host" }));
    expect(screen.getByRole("button", { name: "Add mapping" })).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Select machine" }));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "local-host" }));

    expect(settingsService.getApiV1SettingsWorktreeMappings).toHaveBeenNthCalledWith(2, {
      machine: "remote-host",
    });
    expect(settingsService.getApiV1SettingsWorktreeMappings).toHaveBeenNthCalledWith(3, {
      machine: "local-host",
    });

    resolveLocal(response("local-host", [mapping({ machine: "local-host", project: "new-local" })]));
    await flush();
    resolveRemote(response("remote-host", [mapping({ project: "stale-remote" })]));
    await flush();

    expect(document.body.textContent).toContain("new-local");
    expect(document.body.textContent).not.toContain("stale-remote");
  });

  it("creates and applies mappings for the selected machine", async () => {
    settingsService.getApiV1SettingsWorktreeMappings
      .mockResolvedValueOnce(response("remote-host"))
      .mockResolvedValue(response("remote-host", [mapping()]));
    settingsService.postApiV1SettingsWorktreeMappings.mockResolvedValue({
      id: 1,
      machine: "remote-host",
      path_prefix: "/tmp/service",
      layout: "repo_dot_worktrees",
      project: "",
      enabled: true,
      created_at: "2026-07-04T00:00:00.000Z",
      updated_at: "2026-07-04T00:00:00.000Z",
    });
    settingsService.putApiV1SettingsWorktreeMappingsId.mockResolvedValue({
      id: 1,
      machine: "test",
      path_prefix: "/tmp/service",
      layout: "explicit",
      project: "service",
      enabled: true,
      created_at: "2026-07-04T00:00:00.000Z",
      updated_at: "2026-07-04T00:00:00.000Z",
    });

    settingsService.postApiV1SettingsWorktreeMappingsApply.mockResolvedValue({
      machine: "remote-host",
      updated_sessions: 2,
      matched_sessions: 3,
    });

    component = mount(WorktreeMappingSettings, {
      target: document.body,
      props: {
        readOnly: false,
      },
    });

    await flush();

    const layoutButton = screen.getByRole("button", { name: /repo\.worktrees/ });
    expect(layoutButton).toBeTruthy();
    layoutButton?.click();
    await new Promise((resolve) => setTimeout(resolve, 0));

    const pathPrefixInput = screen.getByRole("textbox", { name: "Parent directory" });
    const projectInput = screen.getByRole("textbox", { name: "Project" }) as HTMLInputElement;
    expect(projectInput.disabled).toBe(true);
    await fireEvent.input(pathPrefixInput, { target: { value: "/tmp/service" } });

    await fireEvent.click(screen.getByRole("button", { name: "Add mapping" }));
    await flush();

    expect(
      settingsService.postApiV1SettingsWorktreeMappings,
    ).toHaveBeenCalledWith({
      requestBody: {
        path_prefix: "/tmp/service",
        layout: "repo_dot_worktrees",
        project: "",
        enabled: true,
        machine: "remote-host",
      },
    });

    await fireEvent.click(screen.getByRole("button", { name: "Apply mappings" }));
    expect(settingsService.postApiV1SettingsWorktreeMappingsApply).toHaveBeenCalledWith({
      requestBody: { machine: "remote-host" },
    });
  });

  it("does not let a completed save overwrite the newly selected machine", async () => {
    const save = deferred<unknown>();
    settingsService.getApiV1SettingsWorktreeMappings
      .mockResolvedValueOnce(response("local-host"))
      .mockResolvedValueOnce(response("remote-host", [mapping({ project: "remote-project" })]));
    settingsService.postApiV1SettingsWorktreeMappings.mockReturnValue(save.promise);

    component = mount(WorktreeMappingSettings, { target: document.body });
    await flush();

    await fireEvent.input(screen.getByRole("textbox", { name: "Path prefix" }), {
      target: { value: "/worktrees/local" },
    });
    await fireEvent.input(screen.getByRole("textbox", { name: "Project" }), {
      target: { value: "local-project" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Add mapping" }));

    await fireEvent.click(screen.getByRole("button", { name: "Select machine" }));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "remote-host" }));
    await flush();
    await fireEvent.input(screen.getByRole("textbox", { name: "Path prefix" }), {
      target: { value: "/worktrees/remote" },
    });
    await fireEvent.input(screen.getByRole("textbox", { name: "Project" }), {
      target: { value: "remote-draft" },
    });

    save.resolve(mapping({ machine: "local-host", project: "local-project" }));
    await flush();

    expect(settingsService.getApiV1SettingsWorktreeMappings).toHaveBeenCalledTimes(2);
    expect(document.body.textContent).toContain("remote-project");
    expect((screen.getByRole("textbox", { name: "Path prefix" }) as HTMLInputElement).value)
      .toBe("/worktrees/remote");
    expect((screen.getByRole("textbox", { name: "Project" }) as HTMLInputElement).value)
      .toBe("remote-draft");
    expect(document.body.textContent).not.toContain("local-project");
  });

  it("does not show a stale apply error after the selected machine changes", async () => {
    const apply = deferred<unknown>();
    settingsService.getApiV1SettingsWorktreeMappings
      .mockResolvedValueOnce(response("local-host", [mapping({ machine: "local-host" })]))
      .mockResolvedValueOnce(response("remote-host", [mapping({ project: "remote-project" })]));
    settingsService.postApiV1SettingsWorktreeMappingsApply.mockReturnValue(apply.promise);

    component = mount(WorktreeMappingSettings, { target: document.body });
    await flush();

    await fireEvent.click(screen.getByRole("button", { name: "Apply mappings" }));
    await fireEvent.click(screen.getByRole("button", { name: "Select machine" }));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "remote-host" }));
    await flush();

    apply.reject(new Error("local apply failed"));
    await flush();

    expect(document.body.textContent).toContain("remote-project");
    expect(document.body.textContent).not.toContain("local apply failed");
    expect((screen.getByRole("button", { name: "Apply mappings" }) as HTMLButtonElement).disabled)
      .toBe(false);
  });

  it("does not show a stale apply result after the selected machine changes", async () => {
    const apply = deferred<unknown>();
    settingsService.getApiV1SettingsWorktreeMappings
      .mockResolvedValueOnce(response("local-host", [mapping({ machine: "local-host" })]))
      .mockResolvedValueOnce(response("remote-host", [mapping({ project: "remote-project" })]));
    settingsService.postApiV1SettingsWorktreeMappingsApply.mockReturnValue(apply.promise);

    component = mount(WorktreeMappingSettings, { target: document.body });
    await flush();

    await fireEvent.click(screen.getByRole("button", { name: "Apply mappings" }));
    await fireEvent.click(screen.getByRole("button", { name: "Select machine" }));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "remote-host" }));
    await flush();

    apply.resolve({ machine: "local-host", updated_sessions: 91, matched_sessions: 92 });
    await flush();

    expect(document.body.textContent).toContain("remote-project");
    expect(document.body.textContent).not.toContain("91 updated, 92 matched");
  });

  it("does not show a stale save error after the selected machine changes", async () => {
    const save = deferred<unknown>();
    settingsService.getApiV1SettingsWorktreeMappings
      .mockResolvedValueOnce(response("local-host"))
      .mockResolvedValueOnce(response("remote-host", [mapping({ project: "remote-project" })]));
    settingsService.postApiV1SettingsWorktreeMappings.mockReturnValue(save.promise);

    component = mount(WorktreeMappingSettings, { target: document.body });
    await flush();

    await fireEvent.input(screen.getByRole("textbox", { name: "Path prefix" }), {
      target: { value: "/worktrees/local" },
    });
    await fireEvent.input(screen.getByRole("textbox", { name: "Project" }), {
      target: { value: "local-project" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Add mapping" }));
    await fireEvent.click(screen.getByRole("button", { name: "Select machine" }));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "remote-host" }));
    await flush();

    save.reject(new Error("local save failed"));
    await flush();

    expect(document.body.textContent).toContain("remote-project");
    expect(document.body.textContent).not.toContain("local save failed");
  });

  it("does not let a completed delete refresh the newly selected machine", async () => {
    const deletion = deferred<unknown>();
    settingsService.getApiV1SettingsWorktreeMappings
      .mockResolvedValueOnce(response("local-host", [mapping({ machine: "local-host" })]))
      .mockResolvedValueOnce(response("remote-host", [mapping({ project: "remote-project" })]));
    settingsService.deleteApiV1SettingsWorktreeMappingsId.mockReturnValue(deletion.promise);

    component = mount(WorktreeMappingSettings, { target: document.body });
    await flush();

    await fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    await fireEvent.click(screen.getByRole("button", { name: "Delete mapping" }));
    await fireEvent.click(screen.getByRole("button", { name: "Select machine" }));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "remote-host" }));
    await flush();

    deletion.resolve(undefined);
    await flush();

    expect(settingsService.getApiV1SettingsWorktreeMappings).toHaveBeenCalledTimes(2);
    expect(document.body.textContent).toContain("remote-project");
  });

  it("shows remembered original labels and confirms delete", async () => {
    settingsService.getApiV1SettingsWorktreeMappings.mockResolvedValue(
      response("remote-host", [mapping()]),
    );
    settingsService.deleteApiV1SettingsWorktreeMappingsId.mockResolvedValue(undefined);
    component = mount(WorktreeMappingSettings, { target: document.body });
    await flush();

    expect(document.body.textContent).toContain("Originally shown as branch-label");
    await fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    expect(settingsService.deleteApiV1SettingsWorktreeMappingsId).not.toHaveBeenCalled();
    expect(screen.getByRole("dialog").textContent).toContain("source files still exist");
    expect(screen.getByRole("dialog").textContent).toContain("Orphaned sessions");

    await fireEvent.click(screen.getByRole("button", { name: "Delete mapping" }));
    expect(settingsService.deleteApiV1SettingsWorktreeMappingsId).toHaveBeenCalledWith({ id: "1" });
  });

  it("confirms an enabled-to-disabled save before updating", async () => {
    settingsService.getApiV1SettingsWorktreeMappings.mockResolvedValue(
      response("remote-host", [mapping()]),
    );
    settingsService.putApiV1SettingsWorktreeMappingsId.mockResolvedValue(mapping({ enabled: false }));
    component = mount(WorktreeMappingSettings, { target: document.body });
    await flush();

    await fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    await fireEvent.click(screen.getByRole("checkbox", { name: "Enabled" }));
    await fireEvent.click(screen.getByRole("button", { name: "Save mapping" }));

    expect(settingsService.putApiV1SettingsWorktreeMappingsId).not.toHaveBeenCalled();
    expect(screen.getByRole("dialog", { name: "Disable mapping?" })).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Disable mapping" }));
    expect(settingsService.putApiV1SettingsWorktreeMappingsId).toHaveBeenCalledWith({
      id: "1",
      requestBody: {
        enabled: false,
        layout: "explicit",
        machine: "remote-host",
        path_prefix: "/srv/worktrees/example",
        project: "canonical-project",
      },
    });
  });
});
