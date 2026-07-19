import {
  expect,
  test,
  type Browser,
  type Locator,
  type Page,
} from "@playwright/test";

const isDuckDBBackend = process.env.AGENTSVIEW_E2E_BACKEND === "duckdb";
const wrongProject = "wrong_branch_label";
const targetProject = "sample_service";
const machine = "remote-example-host";
const worktreeRoot =
  "/srv/worktrees/github.com/example-org/sample-service/example-worktree";
const broaderPrefix = "/srv/worktrees/github.com/example-org/sample-service";

async function openFixtureActivity(page: Page): Promise<void> {
  await page.goto("/activity?window_days=40");
  await expect(
    page.getByRole("button", { name: `Reclassify project ${wrongProject}` }),
  ).toBeAttached();
}

function projectAction(page: Page) {
  return page.getByRole("button", {
    name: `Reclassify project ${wrongProject}`,
  });
}

async function tabTo(page: Page, target: Locator, limit = 100): Promise<void> {
  for (let i = 0; i < limit; i += 1) {
    await page.keyboard.press("Tab");
    if (await target.evaluate((element) => element === document.activeElement)) {
      return;
    }
  }
  throw new Error(`Target was not reached after ${limit} Tab presses`);
}

async function expectActionOpacity(
  page: Page,
  expected: string,
): Promise<void> {
  await expect(projectAction(page).locator("xpath=.."))
    .toHaveCSS("opacity", expected);
}

async function expectCoarsePointerActionVisible(
  browser: Browser,
): Promise<void> {
  const context = await browser.newContext({
    hasTouch: true,
    isMobile: true,
    viewport: { width: 900, height: 900 },
  });
  try {
    const page = await context.newPage();
    await openFixtureActivity(page);
    expect(await page.evaluate(() => matchMedia("(pointer: coarse)").matches))
      .toBe(true);
    await expectActionOpacity(page, "1");
  } finally {
    await context.close();
  }
}

test.describe("Activity project reclassification", () => {
  test.skip(
    ({ browserName }) => browserName !== "chromium",
    "the workflow mutates the shared fixture once and coarse-pointer coverage targets Chromium",
  );

  test("keeps the project action visible for a real coarse pointer", async ({
    browser,
  }) => {
    await expectCoarsePointerActionVisible(browser);
  });

  test("reclassifies a remote worktree and exposes its persisted rule", async ({
    page,
  }) => {
    test.skip(isDuckDBBackend, "requires the writable SQLite archive");

    let previewRequests = 0;
    let reclassifyMutations = 0;
    page.on("request", (request) => {
      if (
        request.method() === "POST" &&
        new URL(request.url()).pathname ===
          "/api/v1/settings/worktree-mappings/preview"
      ) {
        previewRequests += 1;
      }
      if (
        request.method() === "POST" &&
        new URL(request.url()).pathname ===
          "/api/v1/settings/worktree-mappings/reclassify"
      ) {
        reclassifyMutations += 1;
      }
    });

    await openFixtureActivity(page);
    const action = projectAction(page);

    await expectActionOpacity(page, "0");
    await action.hover();
    await expectActionOpacity(page, "1");

    // The revealed action occupies its own reserved slot; it must not cover
    // the row's value column.
    const actionBox = await action.boundingBox();
    const valueBox = await page
      .locator(".bar-row", { has: action })
      .locator(".bar-value")
      .boundingBox();
    expect(actionBox).not.toBeNull();
    expect(valueBox).not.toBeNull();
    expect(actionBox!.x).toBeGreaterThanOrEqual(valueBox!.x + valueBox!.width);

    await page.mouse.move(0, 0);
    await tabTo(page, action);
    await expect(action).toBeFocused();
    await expectActionOpacity(page, "1");
    await page.keyboard.press("Enter");

    const dialog = page.getByRole("dialog", { name: "Reclassify project" });
    await expect(dialog).toBeVisible();
    await expect(dialog.getByText(`Originally shown as ${wrongProject}`))
      .toBeVisible();
    await expect(dialog.getByText(machine, { exact: true })).toBeVisible();
    await expect(dialog.getByText("2 sessions", { exact: true })).toBeVisible();

    const prefix = dialog.getByRole("textbox", { name: "Path prefix" });
    await expect(prefix).toHaveValue(worktreeRoot);
    await prefix.fill(broaderPrefix);

    await dialog.getByTitle("Target project").click();
    const targetInput = dialog.getByRole("combobox");
    await targetInput.fill(targetProject);
    await dialog
      .getByRole("option", { name: `Use project "${targetProject}"` })
      .click();

    await expect.poll(() => previewRequests).toBe(1);
    await expect(dialog.getByText("Full archive impact")).toBeVisible();
    await expect(dialog.getByText("2 sessions matched", { exact: true }))
      .toBeVisible();
    await expect(dialog.getByText("2 sessions will change", { exact: true }))
      .toBeVisible();
    await expect(dialog.getByText("1 project", { exact: true }))
      .toBeVisible();

    await dialog.getByRole("button", { name: "Apply reclassification" }).click();
    await expect(dialog).toBeHidden();
    expect(reclassifyMutations).toBe(1);

    await expect(
      page.getByRole("button", {
        name: `Reclassify project ${targetProject}`,
      }),
    ).toBeAttached();
    await expect(projectAction(page)).toHaveCount(0);
    await expect(page.getByRole("heading", { name: "Project", exact: true }))
      .toBeFocused();

    await page.getByRole("button", { name: "Settings", exact: true }).click();
    await expect(page.getByText("Worktree mappings", { exact: true }))
      .toBeVisible();
    await page.getByRole("button", { name: "Select machine" }).click();
    await page.getByRole("option", { name: machine, exact: true }).click();

    await expect(page.getByText(targetProject, { exact: true }))
      .toBeVisible();
    await expect(page.getByText(broaderPrefix, { exact: true }))
      .toBeVisible();
    await expect(
      page.getByText(`Originally shown as ${wrongProject}`, {
        exact: true,
      }),
    ).toBeVisible();
  });

  test("explains read-only reclassification without making workflow requests", async ({
    page,
  }) => {
    test.skip(!isDuckDBBackend, "runs only against duckdb serve");

    const workflowRequests: string[] = [];
    page.on("request", (request) => {
      const pathname = new URL(request.url()).pathname;
      if (
        pathname.includes("project-reclassification/candidates") ||
        pathname.includes("worktree-mappings/preview") ||
        pathname.includes("worktree-mappings/reclassify")
      ) {
        workflowRequests.push(`${request.method()} ${pathname}`);
      }
    });

    await openFixtureActivity(page);
    const action = projectAction(page);
    await expect(action).toHaveAttribute("aria-disabled", "true");
    await expect(action).toHaveAttribute(
      "title",
      "Reclassification is available from the writable archive that syncs this machine's sessions.",
    );

    await tabTo(page, action);
    await expect(action).toBeFocused();
    await expectActionOpacity(page, "1");
    await page.keyboard.press("Enter");

    await expect(page.getByRole("dialog", { name: "Reclassify project" }))
      .toHaveCount(0);
    expect(workflowRequests).toEqual([]);
  });
});
