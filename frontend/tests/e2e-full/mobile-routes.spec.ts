import { devices, expect, test, type Page } from "@playwright/test";
import { startIsolatedE2EServerWithOptions } from "./support/e2eServer.js";

const iPhone13 = devices["iPhone 13"];
test.use({
  viewport: iPhone13.viewport,
  deviceScaleFactor: iPhone13.deviceScaleFactor,
  userAgent: iPhone13.userAgent,
});

async function expectPathname(page: Page, pathname: string): Promise<void> {
  await expect.poll(() => new URL(page.url()).pathname).toBe(pathname);
}

async function mobileSearchBarMetrics(page: Page) {
  return await page.evaluate(() => {
    const bar = document.querySelector(".mobile-triage-search-bar");
    const search = bar?.querySelector(".kit-search-input");
    const filter = bar?.querySelector(".kit-icon-button");
    const barStyle = bar ? getComputedStyle(bar) : null;
    const searchStyle = search ? getComputedStyle(search) : null;
    const filterStyle = filter ? getComputedStyle(filter) : null;
    const rect = (element: Element | null | undefined) => {
      const bounds = element?.getBoundingClientRect();
      return bounds ? { width: bounds.width, height: bounds.height } : null;
    };
    return {
      bar: rect(bar),
      search: rect(search),
      filter: rect(filter),
      gap: barStyle?.gap,
      padding: barStyle
        ? [barStyle.paddingTop, barStyle.paddingRight, barStyle.paddingBottom, barStyle.paddingLeft]
        : null,
      searchRadius: searchStyle?.borderRadius,
      filterRadius: filterStyle?.borderRadius,
      searchBackground: searchStyle?.backgroundColor,
      filterBackground: filterStyle?.backgroundColor,
    };
  });
}

async function expectReadableFocusList(page: Page, itemSelector: string): Promise<void> {
  await expect(page.locator(itemSelector).first()).toBeVisible();

  const metrics = await page.evaluate((selector) => {
    const fontSize = (node: Element | null): number => (node ? Number.parseFloat(getComputedStyle(node).fontSize) : 0);
    const rect = (node: Element | null): DOMRect | null => node?.getBoundingClientRect() ?? null;
    const compactRect = (node: Element | null) => {
      const r = rect(node);
      return r ? { left: r.left, right: r.right, height: r.height } : null;
    };
    const item = document.querySelector(selector);
    const title = item?.querySelector(".title") ?? null;
    const meta = item?.querySelector(".meta-text") ?? null;
    const search = document.querySelector(".focus-list .kit-search-input");
    const stateButton = document.querySelector(".focus-list .state-btn");
    const focusList = document.querySelector(".focus-list");
    const tokenValue = (node: Element | null, name: string): string =>
      node ? getComputedStyle(node).getPropertyValue(name).trim() : "";

    return {
      viewportWidth: window.innerWidth,
      documentWidth: document.documentElement.scrollWidth,
      mobileTypeToken: tokenValue(focusList, "--mobile-type-body"),
      focusHitTarget: tokenValue(focusList, "--focus-mobile-hit-target"),
      searchFontSize: fontSize(search),
      stateButtonFontSize: fontSize(stateButton),
      stateButtonRect: compactRect(stateButton),
      itemFontSize: fontSize(item),
      itemRect: compactRect(item),
      titleFontSize: fontSize(title),
      metaFontSize: fontSize(meta),
      itemBounds: [...document.querySelectorAll(selector)].slice(0, 6).map(compactRect),
    };
  }, itemSelector);

  expect(metrics.mobileTypeToken).toBe("1rem");
  expect(metrics.focusHitTarget).toMatch(/px$/);
  expect(metrics.documentWidth).toBeLessThanOrEqual(metrics.viewportWidth);
  expect(metrics.searchFontSize).toBeGreaterThanOrEqual(16);
  expect(metrics.stateButtonFontSize).toBeGreaterThanOrEqual(15);
  expect(metrics.stateButtonRect?.height ?? 0).toBeGreaterThanOrEqual(44);
  expect(metrics.itemFontSize).toBeGreaterThanOrEqual(16);
  expect(metrics.itemRect?.height ?? 0).toBeGreaterThanOrEqual(72);
  expect(metrics.titleFontSize).toBeGreaterThanOrEqual(19);
  expect(metrics.metaFontSize).toBeGreaterThanOrEqual(15);
  for (const bounds of metrics.itemBounds) {
    expect(bounds?.left ?? 0).toBeGreaterThanOrEqual(0);
    expect(bounds?.right ?? 0).toBeLessThanOrEqual(metrics.viewportWidth);
  }
}

async function expectReadableDetail(page: Page): Promise<void> {
  await expect(
    page.locator(".focus-layout .pull-detail .detail-title, .focus-layout .issue-detail .detail-title"),
  ).toBeVisible();

  const metrics = await page.evaluate(() => {
    const detail = document.querySelector(".pull-detail, .issue-detail");
    const layout = document.querySelector(".focus-layout");
    const fontSize = (selector: string): number => {
      const node = document.querySelector(selector);
      return node ? Number.parseFloat(getComputedStyle(node).fontSize) : 0;
    };
    const rect = (selector: string) => {
      const r = document.querySelector(selector)?.getBoundingClientRect();
      return r ? { left: r.left, right: r.right, height: r.height } : null;
    };
    const tokenValue = (node: Element | null, name: string): string =>
      node ? getComputedStyle(node).getPropertyValue(name).trim() : "";
    const overflowingVisible = [...document.querySelectorAll(".focus-layout *")]
      .filter((el) => {
        const r = el.getBoundingClientRect();
        return r.width > 0 && r.height > 0 && r.left < window.innerWidth && r.right > window.innerWidth + 0.5;
      })
      .map((el) => el.className?.toString() || el.tagName.toLowerCase());

    return {
      viewportWidth: window.innerWidth,
      documentWidth: document.documentElement.scrollWidth,
      detailTypeToken: tokenValue(detail, "--detail-mobile-type-body"),
      detailHitTarget: tokenValue(detail, "--detail-mobile-hit-target"),
      mobileTypeToken: tokenValue(layout, "--mobile-type-body"),
      rootFontSize: Number.parseFloat(getComputedStyle(document.documentElement).fontSize),
      titleFontSize: fontSize(".detail-title"),
      metaFontSize: fontSize(".meta-item"),
      bodyFontSize: fontSize(".pull-detail, .issue-detail"),
      chipFontSize: fontSize(".kit-chip, .state-chip, .status-chip"),
      copyNumberFontSize: fontSize(".copy-number-btn"),
      copyNumberRect: rect(".copy-number-btn"),
      overflowingVisible,
    };
  });

  expect(metrics.detailTypeToken).not.toBe("");
  expect(metrics.detailHitTarget).toMatch(/px$/);
  expect(metrics.mobileTypeToken).toBe("1rem");
  expect(metrics.rootFontSize).toBe(16);
  expect(metrics.documentWidth).toBeLessThanOrEqual(metrics.viewportWidth);
  expect(metrics.titleFontSize).toBeGreaterThanOrEqual(19);
  expect(metrics.metaFontSize).toBeGreaterThanOrEqual(15);
  expect(metrics.bodyFontSize).toBeGreaterThanOrEqual(16);
  expect(metrics.chipFontSize).toBeGreaterThanOrEqual(14);
  expect(metrics.copyNumberFontSize).toBeGreaterThanOrEqual(15);
  // The number button sits inside a text row, so it keeps text sizing with the
  // WCAG 2.5.8 24px target floor rather than the standalone phone hit target.
  expect(metrics.copyNumberRect?.height ?? 0).toBeGreaterThanOrEqual(24);
  expect(metrics.overflowingVisible).toEqual([]);
}

test.describe("phone routes", () => {
  test("phone viewport visiting desktop root renders mobile activity without changing URL", async ({ page }) => {
    await page.goto("/");

    await expectPathname(page, "/");
    await expect(page.locator(".mobile-shell")).toBeVisible();
    await expect(page.getByRole("combobox", { name: /Phone mode/ })).toHaveText("Activity");
    await expect(page.locator(".mobile-topbar .mobile-app-icon")).toBeVisible();
    await expect(page.getByRole("button", { name: "Open desktop view" })).toBeVisible();
    await expect(page.locator(".app-top-bar")).toHaveCount(0);
    await expect(page.locator("footer")).toHaveCount(0);

    const metrics = await page.evaluate(() => {
      const search = document.querySelector(".kit-search-input");
      const rect = search?.getBoundingClientRect();
      return {
        viewportWidth: window.innerWidth,
        documentWidth: document.documentElement.scrollWidth,
        searchLeft: rect?.left ?? 0,
        searchRight: rect?.right ?? 0,
      };
    });

    expect(metrics.documentWidth).toBeLessThanOrEqual(metrics.viewportWidth);
    expect(metrics.searchLeft).toBeGreaterThanOrEqual(0);
    expect(metrics.searchRight).toBeLessThanOrEqual(metrics.viewportWidth);
  });

  test("mobile activity uses a phone-first inbox rather than the desktop threaded list", async ({ page }) => {
    await page.goto("/m?range=30d&view=threaded");

    await expect(page.locator(".mobile-shell")).toBeVisible();
    await expect(page.locator(".mobile-activity-inbox")).toBeVisible();
    await expect(page.locator(".mobile-triage-search-bar")).toBeVisible();
    await expect(page.getByText("Readable threads first")).toHaveCount(0);
    const search = page.getByPlaceholder("Search activity");
    const filters = page.getByRole("button", { name: /^Filters/ });
    const activityFilters = page.locator("#mobile-activity-filters");
    await expect(filters).toHaveText("");
    await expect(filters).toHaveAttribute("aria-expanded", "false");
    await expect(activityFilters).toBeAttached();
    await expect(activityFilters).toBeHidden();
    const [searchBounds, filterBounds] = await Promise.all([search.boundingBox(), filters.boundingBox()]);
    expect(searchBounds).not.toBeNull();
    expect(filterBounds).not.toBeNull();
    expect(Math.abs(searchBounds!.y - filterBounds!.y)).toBeLessThan(2);
    expect(filterBounds!.height).toBeGreaterThanOrEqual(44);
    await filters.locator("svg").click();
    await expect(filters).toHaveAttribute("aria-expanded", "true");
    await expect(activityFilters).toBeVisible();
    expect((await activityFilters.boundingBox())?.height ?? 0).toBeGreaterThan(0);
    await expect(page.getByRole("switch", { name: "PRs" })).toBeVisible();
    await expect(page.getByRole("switch", { name: "Issues" })).toBeVisible();
    await expect(page.getByRole("switch", { name: "Comments" })).toBeVisible();
    await expect(page.getByRole("switch", { name: "Reviews" })).toBeVisible();
    await expect(page.getByRole("switch", { name: "Commits" })).toBeVisible();
    await expect(page.getByRole("switch", { name: "Force pushes" })).toBeVisible();
    await expect(page.getByRole("switch", { name: "Notifications" })).toBeVisible();
    await expect(page.getByRole("switch", { name: "Hide closed/merged" })).toBeVisible();
    await expect(page.getByLabel("Time range")).toBeVisible();
    await expect(page.getByRole("button", { name: /^Select repository:/ })).toBeVisible();
    await expect(page.locator(".threaded-view")).toHaveCount(0);

    const metrics = await page.evaluate(() => {
      const firstCard = document.querySelector(".mobile-activity-card");
      const firstButton = document.querySelector(".mobile-activity-card button");
      const title = document.querySelector(".mobile-activity-card__title");
      const meta = document.querySelector(".mobile-activity-card__meta");
      const metaItems = meta?.querySelectorAll("span") ?? [];
      const metaRect = meta?.getBoundingClientRect();
      const metaCountRect = metaItems[1]?.getBoundingClientRect();
      const eventLabel = document.querySelector(".mobile-activity-event__body strong");
      const eventAuthor = document.querySelector(".mobile-activity-event__body span");
      const eventTime = document.querySelector(".mobile-activity-event time");
      const mobileBrandLabel = document.querySelector(".forge-selector-fallback");
      const mobileModePicker = document.querySelector(".mobile-mode-picker .kit-select-dropdown__trigger");
      const desktopButton = document.querySelector(".mobile-desktop-link");
      const desktopIcon = document.querySelector(".mobile-desktop-link svg");
      const appIcon = document.querySelector(".mobile-app-icon");
      const itemTypeToggle = document.querySelector(".mobile-item-type-toggle .kit-toggle");
      const rangeSelect = document.querySelector(".mobile-filter-dropdown button[aria-label^='Time range']");
      const repoSelect = document.querySelector(".mobile-filter-select--repo .typeahead-trigger");
      const repoChevron = repoSelect?.querySelector(".typeahead-chevron") ?? null;
      const toolbar = document.querySelector(".mobile-triage-search-bar");
      const filterPanel = document.querySelector(".mobile-activity-filter-grid");
      const authorFilter = document.querySelector(".mobile-author-filter");
      const authorChevron = authorFilter?.querySelector(".kit-typeahead__chevron") ?? null;
      const search = document.querySelector(".kit-search-input");
      const cardRect = firstCard?.getBoundingClientRect();
      const buttonRect = firstButton?.getBoundingClientRect();
      const searchRect = search?.getBoundingClientRect();
      const styleFor = (node: Element | null) => (node ? getComputedStyle(node) : null);
      const themeSample = document.createElement("div");
      themeSample.style.cssText = [
        "position:absolute",
        "left:-9999px",
        "top:0",
        "width:1px",
        "height:1px",
        "background:var(--bg-primary)",
        "border-color:var(--border-default)",
      ].join(";");
      document.body.append(themeSample);
      const surfaceSample = document.createElement("div");
      surfaceSample.style.cssText = [
        "position:absolute",
        "left:-9999px",
        "top:0",
        "width:1px",
        "height:1px",
        "background:var(--bg-surface)",
        "border-radius:var(--radius-lg)",
      ].join(";");
      document.body.append(surfaceSample);
      const compactRect = (node: Element | null) => {
        const r = node?.getBoundingClientRect();
        return r ? { top: r.top, left: r.left, right: r.right, width: r.width, height: r.height } : null;
      };
      const fontSize = (node: Element | null): number =>
        node ? Number.parseFloat(getComputedStyle(node).fontSize) : 0;
      return {
        viewportWidth: window.innerWidth,
        documentWidth: document.documentElement.scrollWidth,
        mobileTypeToken: getComputedStyle(document.querySelector(".mobile-shell") ?? document.documentElement)
          .getPropertyValue("--mobile-type-body")
          .trim(),
        titleTypeToken: getComputedStyle(document.querySelector(".mobile-shell") ?? document.documentElement)
          .getPropertyValue("--mobile-type-title")
          .trim(),
        rootFontSize: Number.parseFloat(getComputedStyle(document.documentElement).fontSize),
        cardHeight: cardRect?.height ?? 0,
        touchTargetHeight: buttonRect?.height ?? 0,
        titleFontSize: fontSize(title),
        metaFontSize: fontSize(meta),
        metaItemCount: metaItems.length,
        metaRight: metaRect?.right ?? 0,
        metaCountRight: metaCountRect?.right ?? 0,
        eventLabelFontSize: fontSize(eventLabel),
        eventAuthorFontSize: fontSize(eventAuthor),
        eventTimeFontSize: fontSize(eventTime),
        mobileBrandFontSize: fontSize(mobileBrandLabel),
        mobileModePickerFontSize: fontSize(mobileModePicker),
        desktopButtonText: desktopButton?.textContent?.trim() ?? "",
        desktopButtonRect: compactRect(desktopButton),
        desktopIconPresent: Boolean(desktopIcon),
        appIconPresent: Boolean(appIcon),
        inboxBackground: styleFor(document.querySelector(".mobile-activity-inbox"))?.backgroundColor ?? "",
        cardBackground: styleFor(firstCard)?.backgroundColor ?? "",
        cardBorderColor: styleFor(firstCard)?.borderColor ?? "",
        cardRadius: styleFor(firstCard)?.borderRadius ?? "",
        toolbarBackground: styleFor(toolbar)?.backgroundColor ?? "",
        toolbarBorderBottom: styleFor(toolbar)?.borderBottomColor ?? "",
        toolbarRect: compactRect(toolbar),
        filterPanelBackground: styleFor(filterPanel)?.backgroundColor ?? "",
        themeBgPrimary: getComputedStyle(themeSample).backgroundColor,
        themeBgSurface: getComputedStyle(surfaceSample).backgroundColor,
        themeBorder: getComputedStyle(themeSample).borderColor,
        themeRadiusLg: getComputedStyle(surfaceSample).borderRadius,
        itemTypeToggleFontSize: fontSize(document.querySelector(".mobile-item-type-toggle .kit-toggle__label")),
        rangeSelectFontSize: fontSize(rangeSelect),
        repoSelectFontSize: fontSize(repoSelect),
        itemTypeToggleRect: compactRect(itemTypeToggle),
        rangeSelectRect: compactRect(rangeSelect),
        repoSelectRect: compactRect(repoSelect),
        repoChevronRect: compactRect(repoChevron),
        authorFilterRect: compactRect(authorFilter),
        authorChevronRect: compactRect(authorChevron),
        searchLeft: searchRect?.left ?? 0,
        searchRight: searchRect?.right ?? 0,
      };
    });

    expect(metrics.documentWidth).toBeLessThanOrEqual(metrics.viewportWidth);
    expect(metrics.searchLeft).toBeGreaterThanOrEqual(0);
    expect(metrics.searchRight).toBeLessThanOrEqual(metrics.viewportWidth);
    expect(metrics.mobileTypeToken).toBe("1rem");
    expect(metrics.titleTypeToken).toBe("1.25rem");
    expect(metrics.rootFontSize).toBe(16);
    expect(metrics.cardHeight).toBeGreaterThanOrEqual(110);
    expect(metrics.touchTargetHeight).toBeGreaterThanOrEqual(44);
    expect(metrics.titleFontSize).toBeGreaterThanOrEqual(19);
    expect(metrics.metaFontSize).toBeGreaterThanOrEqual(15);
    expect(metrics.metaItemCount).toBe(2);
    expect(Math.abs(metrics.metaRight - metrics.metaCountRight)).toBeLessThanOrEqual(1);
    expect(metrics.eventLabelFontSize).toBeGreaterThanOrEqual(15);
    expect(metrics.eventAuthorFontSize).toBeGreaterThanOrEqual(14);
    expect(metrics.eventTimeFontSize).toBeGreaterThanOrEqual(14);
    expect(metrics.mobileBrandFontSize).toBeGreaterThanOrEqual(16);
    expect(metrics.mobileModePickerFontSize).toBeGreaterThanOrEqual(16);
    expect(metrics.desktopButtonText).toBe("");
    expect(metrics.desktopButtonRect?.height ?? 0).toBeGreaterThanOrEqual(44);
    expect(metrics.desktopIconPresent).toBe(true);
    expect(metrics.appIconPresent).toBe(true);
    expect(metrics.inboxBackground).toBe(metrics.themeBgPrimary);
    expect(metrics.cardBackground).toBe(metrics.themeBgSurface);
    expect(metrics.cardBorderColor).toBe(metrics.themeBorder);
    expect(metrics.cardRadius).toBe(metrics.themeRadiusLg);
    expect(metrics.toolbarBackground).toBe(metrics.themeBgSurface);
    expect(metrics.toolbarBorderBottom).toBe(metrics.themeBorder);
    expect(metrics.toolbarRect?.left ?? -1).toBe(0);
    expect(metrics.toolbarRect?.right ?? Infinity).toBe(metrics.viewportWidth);
    expect(metrics.toolbarRect?.height ?? 0).toBeGreaterThanOrEqual(64);
    expect(metrics.toolbarRect?.height ?? Infinity).toBeLessThanOrEqual(66);
    expect(metrics.filterPanelBackground).toBe(metrics.themeBgSurface);
    expect(metrics.itemTypeToggleFontSize).toBeGreaterThanOrEqual(15);
    expect(metrics.rangeSelectFontSize).toBeGreaterThanOrEqual(15);
    expect(metrics.repoSelectFontSize).toBeGreaterThanOrEqual(15);
    expect(metrics.repoSelectRect?.top ?? Infinity).toBeLessThan(metrics.authorFilterRect?.top ?? 0);
    expect(
      (metrics.authorFilterRect?.top ?? Infinity) -
        (metrics.repoSelectRect?.top ?? 0) -
        (metrics.repoSelectRect?.height ?? 0),
    ).toBeLessThanOrEqual(1);
    expect(metrics.authorChevronRect?.width).toBe(metrics.repoChevronRect?.width);
    expect(metrics.authorChevronRect?.height).toBe(metrics.repoChevronRect?.height);
    for (const bounds of [metrics.itemTypeToggleRect, metrics.rangeSelectRect, metrics.repoSelectRect]) {
      expect(bounds?.left ?? 0).toBeGreaterThanOrEqual(0);
      expect(bounds?.right ?? 0).toBeLessThanOrEqual(metrics.viewportWidth);
    }
  });

  test("mobile activity filters can narrow by type, range, and repository", async ({ page }) => {
    await page.goto("/m?range=30d&view=threaded");
    await expect(page.locator(".mobile-activity-inbox")).toBeVisible();
    await page.getByRole("button", { name: /^Filters/ }).click();

    await page.getByRole("switch", { name: "Issues" }).click();
    await expect(page.getByRole("switch", { name: "Issues" })).not.toBeChecked();
    await expect(page).toHaveURL(/item_types=pr/);

    await page.getByRole("combobox", { name: /Time range/ }).click();
    await page.getByRole("option", { name: "24h" }).click();
    await expect(page.getByRole("combobox", { name: "Time range: 24h" })).toBeVisible();
    await expect(page).toHaveURL(/range=24h/);

    const activityForRepo = page.waitForResponse((response) => {
      const url = new URL(response.url());
      return url.pathname === "/api/v1/activity" && url.searchParams.get("repo") === "github|github.com/acme/widgets";
    });
    await page.getByRole("button", { name: /^Select repository:/ }).click();
    await page.getByRole("option", { name: "github/github.com/acme/widgets", exact: true }).dispatchEvent("mousedown");
    await page.getByRole("textbox", { name: "Filter repos" }).press("Escape");
    await expect(page.getByRole("button", { name: /^Select repository:/ })).toContainText("acme/widgets");
    expect((await activityForRepo).ok()).toBe(true);

    const repoLabels = page.locator(".mobile-activity-card__meta > span:first-child");
    await expect(repoLabels.first()).toBeVisible();
    expect(new Set(await repoLabels.allTextContents())).toEqual(new Set(["acme/widgets"]));
  });

  test("mobile activity filters by author without overflowing the phone viewport", async ({ page }) => {
    await page.goto("/m?range=30d&view=threaded");
    await expect(page.locator(".mobile-activity-inbox")).toBeVisible();

    const filters = page.getByRole("button", { name: /^Filters/ });
    await expect(filters).toHaveAttribute("aria-expanded", "false");
    await filters.click();
    await expect(filters).toHaveAttribute("aria-expanded", "true");

    await page.getByRole("button", { name: "Filter authors" }).click();
    await expect(page.getByRole("combobox", { name: "Filter authors" })).toBeVisible();
    await expect(page.getByRole("option", { name: "carol" })).toBeVisible();

    const overlayMetrics = await page.locator(".mobile-author-filter .kit-typeahead__panel").evaluate((panel) => {
      const bounds = panel.getBoundingClientRect();
      return {
        viewportWidth: window.innerWidth,
        documentWidth: document.documentElement.scrollWidth,
        left: bounds.left,
        right: bounds.right,
      };
    });
    expect(overlayMetrics.documentWidth).toBeLessThanOrEqual(overlayMetrics.viewportWidth);
    expect(overlayMetrics.left).toBeGreaterThanOrEqual(0);
    expect(overlayMetrics.right).toBeLessThanOrEqual(overlayMetrics.viewportWidth);

    const filteredResponse = page.waitForResponse((response) => {
      const url = new URL(response.url());
      return url.pathname === "/api/v1/activity" && url.searchParams.get("author") === "carol";
    });
    await page.getByRole("option", { name: "carol" }).click();
    const response = await filteredResponse;
    expect(response.status()).toBe(200);
    const payload = await response.json();
    expect(payload.item_activity.length).toBeGreaterThan(0);
    expect(
      payload.item_activity.every((item: { item_author: string }) => item.item_author.toLowerCase() === "carol"),
    ).toBe(true);

    await expect(page).toHaveURL(/author=carol/);
    await expect(page.getByRole("button", { name: "Filters · carol" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Clear author filter carol" })).toHaveCount(0);

    const summaryMetrics = await page.locator(".mobile-triage-search-bar__filter").evaluate((summary) => {
      const bounds = summary.getBoundingClientRect();
      return {
        viewportWidth: window.innerWidth,
        documentWidth: document.documentElement.scrollWidth,
        left: bounds.left,
        right: bounds.right,
      };
    });
    expect(summaryMetrics.documentWidth).toBeLessThanOrEqual(summaryMetrics.viewportWidth);
    expect(summaryMetrics.left).toBeGreaterThanOrEqual(0);
    expect(summaryMetrics.right).toBeLessThanOrEqual(summaryMetrics.viewportWidth);

    const unfilteredResponse = page.waitForResponse((response) => {
      const url = new URL(response.url());
      return url.pathname === "/api/v1/activity" && !url.searchParams.has("author");
    });
    await page.locator(".mobile-author-filter .kit-typeahead__trigger").click();
    await page.getByRole("option", { name: "Anyone" }).click();
    expect((await unfilteredResponse).status()).toBe(200);

    await expect(page.getByRole("button", { name: "Filters" })).toBeVisible();
    await expect.poll(() => new URL(page.url()).searchParams.has("author")).toBe(false);
    const eventAuthors = page.locator(".mobile-activity-event__body > span");
    await expect(async () => {
      const authors = await eventAuthors.allTextContents();
      expect(authors.some((author) => author.trim().toLowerCase() !== "carol")).toBe(true);
    }).toPass({ timeout: 10_000 });
  });

  test("mobile activity hide-org toggle updates card repo labels and persists", async ({ page }) => {
    await page.addInitScript(() => {
      if (sessionStorage.getItem("kenn-forge:test:mobile-hide-org:init") === "1") {
        return;
      }
      localStorage.removeItem("kenn-forge:hideOrgName");
      sessionStorage.setItem("kenn-forge:test:mobile-hide-org:init", "1");
    });

    await page.goto("/m?range=30d&view=threaded");
    await expect(page.locator(".mobile-activity-inbox")).toBeVisible();
    await page.getByRole("button", { name: /^Filters/ }).click();

    const card = page
      .locator(".mobile-activity-card", {
        has: page.locator(".mobile-activity-card__title", {
          hasText: "Add widget caching layer",
        }),
      })
      .filter({
        has: page.locator(".mobile-activity-card__meta span", { hasText: /^(acme\/)?widgets$/ }),
      })
      .first();
    await expect(card).toBeVisible({ timeout: 10_000 });

    const repoLabel = card.locator(".mobile-activity-card__meta span").first();
    const hideOrgToggle = page.getByRole("switch", {
      name: "Hide org",
    });
    await expect(hideOrgToggle).not.toBeChecked();
    await expect(repoLabel).toHaveText("acme/widgets");

    await hideOrgToggle.click();

    await expect(hideOrgToggle).toBeChecked();
    await expect(repoLabel).toHaveText("widgets");
    await expect(repoLabel).not.toHaveText("acme/widgets");

    await page.reload();

    await expect(card).toBeVisible({ timeout: 10_000 });
    await page.getByRole("button", { name: /^Filters/ }).click();
    await expect(hideOrgToggle).toBeChecked();
    await expect(repoLabel).toHaveText("widgets");
  });

  test("mobile activity card routes to a focused thread detail", async ({ page }) => {
    await page.goto("/m?range=30d&view=threaded");
    await expect(page.getByRole("button", { name: "Open thread" })).toHaveCount(0);
    const card = page
      .locator(".mobile-activity-card")
      .filter({
        has: page.locator(".mobile-activity-card__meta span", { hasText: /^acme\/widgets$/ }),
      })
      .filter({ hasText: "Add widget caching layer" })
      .first();
    await expect(card).toBeVisible();

    await card.locator(".mobile-activity-card__button").click();

    await expect(page).toHaveURL(/\/focus\/(?:host\/[^/]+\/)?(?:pulls|issues)\//);
    await expect(page.locator(".focus-layout")).toBeVisible();
    await expect(
      page.locator(".focus-layout .pull-detail .detail-title, .focus-layout .issue-detail .detail-title"),
    ).toBeVisible();
    await expectReadableDetail(page);
    await expect(page.locator(".mobile-shell .mobile-topbar")).toBeVisible();
    await expect(page.locator(".mobile-detail-header__back")).toHaveText("Activity");

    await page.locator(".mobile-detail-header__back").click();

    await expect(page).toHaveURL(/\/m(\?|$)/);
    await expect(page.locator(".mobile-activity-card__button").first()).toBeVisible();
  });

  test("canonical activity phone presentation opens thread detail", async ({ page }) => {
    await page.goto("/");
    await expectPathname(page, "/");
    await expect(page.locator(".mobile-shell")).toBeVisible();
    await expect(page.getByRole("button", { name: "Open thread" })).toHaveCount(0);
    const card = page
      .locator(".mobile-activity-card")
      .filter({
        has: page.locator(".mobile-activity-card__meta span", { hasText: /^acme\/widgets$/ }),
      })
      .filter({ hasText: "Add widget caching layer" })
      .first();
    await expect(card).toBeVisible();

    await card.locator(".mobile-activity-card__button").click();

    await expect(page).toHaveURL(/\/focus\/(?:host\/[^/]+\/)?(?:pulls|issues)\//);
    await expect(page.locator(".focus-layout")).toBeVisible();
    await expect(
      page.locator(".focus-layout .pull-detail .detail-title, .focus-layout .issue-detail .detail-title"),
    ).toBeVisible();
    await expect(page.locator(".mobile-shell .mobile-topbar")).toBeVisible();
    await expect(page.locator(".mobile-detail-header__back")).toHaveText("Activity");
  });

  test("focused PR files tab stays on the phone detail route", async ({ page }) => {
    await page.goto("/focus/pulls/github/acme/widgets/1");
    await expect(page.locator(".focus-layout .pull-detail .detail-title")).toBeVisible();

    await page.locator(".focus-layout").getByRole("tab", { name: "Files changed" }).click();

    await expect(page).toHaveURL(/\/focus\/pulls\/github\/acme\/widgets\/1\/files$/);
    await expect(page.locator(".focus-layout .files-layout")).toBeVisible();
    await expect(page.locator(".focus-layout .diff-view")).toBeVisible();
    await expect(page.locator(".mobile-shell .mobile-detail-header__badge")).toHaveText("PR #1");
  });

  test("phone canonical PR files tab stays on the canonical detail route", async ({ page }) => {
    await page.goto("/pulls/github/acme/widgets/1");
    await expectPathname(page, "/pulls/github/acme/widgets/1");
    await expect(page.locator(".focus-layout .pull-detail .detail-title")).toBeVisible();

    await page.locator(".focus-layout").getByRole("tab", { name: "Files changed" }).click();

    await expectPathname(page, "/pulls/github/acme/widgets/1/files");
    await expect(page.locator(".focus-layout .files-layout")).toBeVisible();
    await expect(page.locator(".focus-layout .diff-view")).toBeVisible();
    await expect(page.locator(".mobile-shell .mobile-detail-header__badge")).toHaveText("PR #1");
  });

  test("long PR branch relationships stay inline with the wrapped head branch", async ({ page }) => {
    await page.route("**/api/v1/pulls/github/acme/widgets/1", async (route) => {
      const response = await route.fetch();
      const detail = await response.json();
      detail.merge_request.HeadBranch = "feature/keep-branch-icon-attached-to-long-wrapped-branch-name";
      await route.fulfill({ response, json: detail });
    });

    await page.goto("/pulls/github/acme/widgets/1");

    const headBranch = page.locator(".meta-branch .branch-name-btn").first();
    const branchIcon = page.locator(".meta-branch .branch-icon");
    await expect(headBranch).toBeVisible();
    await expect(branchIcon).toBeVisible();

    const alignment = await page.locator(".meta-branch").evaluate((metaBranch) => {
      const icon = metaBranch.querySelector(".branch-icon");
      const head = metaBranch.querySelector(".branch-name-btn--head");
      const target = metaBranch.querySelector(".branch-target");
      if (!icon || !head || !target) throw new Error("branch metadata is incomplete");

      const iconBounds = icon.getBoundingClientRect();
      const headText = [...head.childNodes].find(
        (node) => node.nodeType === Node.TEXT_NODE && node.textContent?.trim(),
      );
      if (!headText) throw new Error("head branch text did not render");
      const headTextRange = document.createRange();
      headTextRange.selectNodeContents(headText);
      const headLines = [...headTextRange.getClientRects()];
      const firstHeadLine = headLines.at(0);
      const lastHeadLine = headLines.at(-1);
      const targetBounds = target.getBoundingClientRect();
      if (!firstHeadLine || !lastHeadLine) throw new Error("head branch did not render");

      return {
        headLineCount: headLines.length,
        iconTop: iconBounds.top,
        iconBottom: iconBounds.bottom,
        firstLineTop: firstHeadLine.top,
        firstLineBottom: firstHeadLine.bottom,
        lastLineTop: lastHeadLine.top,
        lastLineRight: lastHeadLine.right,
        targetTop: targetBounds.top,
        targetLeft: targetBounds.left,
      };
    });

    expect(alignment.headLineCount).toBeGreaterThan(1);
    expect(alignment.iconTop).toBeGreaterThanOrEqual(alignment.firstLineTop - 1);
    expect(alignment.iconBottom).toBeLessThanOrEqual(alignment.firstLineBottom + 1);
    expect(Math.abs(alignment.targetTop - alignment.lastLineTop)).toBeLessThan(5);
    expect(alignment.targetLeft).toBeGreaterThanOrEqual(alignment.lastLineRight - 1);
    expect(alignment.targetLeft - alignment.lastLineRight).toBeLessThan(16);
  });

  test("long description collapse toggle has a phone-sized hit target", async ({ page }) => {
    const server = await startIsolatedE2EServerWithOptions();
    try {
      await page.goto(`${server.info.base_url}/pulls/github/acme/widgets/1`);
      await expect(page.locator(".focus-layout .pull-detail .detail-title")).toBeVisible();
      await expect(page.locator(".pull-detail .sync-indicator")).toHaveCount(0, { timeout: 15_000 });

      await page.locator(".edit-body-btn").click();
      await page.locator(".body-edit-textarea").fill(["Long phone description", "", "Details ".repeat(200)].join("\n"));
      await page.locator(".body-edit .title-edit-save").click();

      const collapseToggle = page.getByRole("button", { name: "Collapse description" });
      await expect(collapseToggle).toBeVisible();
      const bounds = await collapseToggle.boundingBox();

      expect(bounds?.width ?? 0).toBeGreaterThanOrEqual(49);
      expect(bounds?.height ?? 0).toBeGreaterThanOrEqual(49);
    } finally {
      await server.stop();
    }
  });

  test("phone canonical PR files deep link renders focus presentation without changing URL", async ({ page }) => {
    await page.goto("/pulls/github/acme/widgets/1/files");

    await expectPathname(page, "/pulls/github/acme/widgets/1/files");
    await expect(page.locator(".focus-layout .files-layout")).toBeVisible();
    await expect(page.locator(".focus-layout .diff-view")).toBeVisible();
    await expect(page.locator(".mobile-shell .mobile-detail-header__badge")).toHaveText("PR #1");
  });

  test("phone canonical issue deep link renders focus presentation without changing URL", async ({ page }) => {
    await page.goto("/issues/github/acme/widgets/10");

    await expectPathname(page, "/issues/github/acme/widgets/10");
    await expect(page.locator(".focus-layout .issue-detail .detail-title")).toBeVisible();
    await expect(page.locator(".mobile-shell .mobile-detail-header__badge")).toHaveText("Issue #10");
  });

  test("phone canonical lists keep the shared filter menu touch-sized and complete", async ({ page }) => {
    for (const route of ["pulls", "issues"]) {
      await page.goto(`/${route}`);
      await expectPathname(page, `/${route}`);
      const trigger = page.getByRole("button", { name: "Filters" });
      await expect(trigger).toBeVisible();
      const bounds = await trigger.boundingBox();
      expect(bounds?.width ?? 0).toBeGreaterThanOrEqual(44);
      expect(bounds?.height ?? 0).toBeGreaterThanOrEqual(44);
      await trigger.click();
      await expect(page.getByRole("button", { name: "Unassigned" })).toBeVisible();
      if (route === "pulls") {
        await expect(page.getByRole("button", { name: "By repo" })).toBeVisible();
      }
    }
  });

  test("phone users can opt out of automatic mobile redirect", async ({ page }) => {
    await page.goto("/?desktop=1");

    await expect(page).toHaveURL(/\/?desktop=1$/);
    await expect(page.locator(".app-top-bar")).toBeVisible();
    await expect(page.locator(".mobile-shell")).toHaveCount(0);
  });

  test("mobile mode picker uses dedicated PR and issue routes", async ({ page }) => {
    await page.goto("/m/pulls");
    await expect(page.locator(".mobile-shell")).toBeVisible();
    const modePicker = page.getByRole("combobox", { name: /Phone mode/ });
    await expect(modePicker).toHaveText("PRs");
    await expect(page.locator(".focus-list")).toBeVisible();
    const pullSearch = page.getByRole("searchbox", { name: "Search PRs" });
    const pullFilters = page.getByRole("button", { name: "Filters" });
    await expect(pullFilters).toHaveText("");
    await expect(pullFilters).toHaveAttribute("aria-expanded", "false");
    await expect(page.locator(".filter-bar")).toBeHidden();
    const [pullSearchBounds, pullFilterBounds] = await Promise.all([
      pullSearch.boundingBox(),
      pullFilters.boundingBox(),
    ]);
    expect(Math.abs(pullSearchBounds!.y - pullFilterBounds!.y)).toBeLessThan(2);
    await pullFilters.click();
    await expect(page.locator(".filter-bar")).toBeVisible();
    await expectReadableFocusList(page, ".pull-item");

    await modePicker.click();
    await page.getByRole("option", { name: "Issues" }).click();
    await expect(page).toHaveURL(/\/m\/issues(?:\?|$)/);
    await expect(modePicker).toHaveText("Issues");
    await expect(page.locator(".focus-list")).toBeVisible();
    const issueSearch = page.getByRole("searchbox", { name: "Search issues" });
    const issueFilters = page.getByRole("button", { name: "Filters" });
    await expect(issueFilters).toHaveAttribute("aria-expanded", "false");
    await expect(page.locator(".filter-bar")).toBeHidden();
    const [issueSearchBounds, issueFilterBounds] = await Promise.all([
      issueSearch.boundingBox(),
      issueFilters.boundingBox(),
    ]);
    expect(Math.abs(issueSearchBounds!.y - issueFilterBounds!.y)).toBeLessThan(2);
    await issueFilters.click();
    await expect(page.locator(".filter-bar")).toBeVisible();
    await expectReadableFocusList(page, ".issue-item");
  });

  test("mobile activity, pull, and issue lists share one search bar geometry", async ({ page }) => {
    await page.goto("/m/pulls");
    await expect(page.getByRole("searchbox", { name: "Search PRs" })).toBeVisible();
    const pullMetrics = await mobileSearchBarMetrics(page);

    await page.goto("/m?view=threaded");
    await expect(page.getByRole("searchbox", { name: "Search activity" })).toBeVisible();
    const activityMetrics = await mobileSearchBarMetrics(page);

    await page.goto("/m/issues");
    await expect(page.getByRole("searchbox", { name: "Search issues" })).toBeVisible();
    const issueMetrics = await mobileSearchBarMetrics(page);

    expect(activityMetrics).toEqual(pullMetrics);
    expect(issueMetrics).toEqual(pullMetrics);
  });

  test("mobile issue bot visibility can be toggled and persists through the real settings API", async ({ page }) => {
    const server = await startIsolatedE2EServerWithOptions();
    const botIssue = page.locator(".issue-item", { hasText: "Security advisory: prototype pollution" });
    const humanIssue = page.locator(".issue-item", { hasText: "Widget rendering broken on Safari" });
    try {
      await page.goto(`${server.info.base_url}/m/issues`);
      await expect(page.locator(".mobile-shell")).toBeVisible();
      await expect(botIssue).toBeVisible();
      await expect(humanIssue).toBeVisible();

      await page.getByRole("button", { name: "Filters" }).click();
      const hideBots = page.getByRole("button", { name: "Hide bot-authored issues" });
      await expect(hideBots).toHaveAttribute("aria-pressed", "false");
      const settingsUpdate = page.waitForResponse(
        (response) => response.url().endsWith("/api/v1/settings") && response.request().method() === "PUT",
      );
      await hideBots.click();
      expect((await settingsUpdate).ok()).toBe(true);
      await expect(hideBots).toHaveAttribute("aria-pressed", "true");
      await expect(botIssue).toHaveCount(0);
      await expect(humanIssue).toBeVisible();

      const settingsResponse = await page.request.get(`${server.info.base_url}/api/v1/settings`);
      expect(settingsResponse.ok()).toBe(true);
      const settings = (await settingsResponse.json()) as { issues: { hide_bots: boolean } };
      expect(settings.issues.hide_bots).toBe(true);

      await page.reload();
      await expect(page.locator(".issue-item").first()).toBeVisible();
      await page.getByRole("button", { name: "Filters" }).click();
      await expect(hideBots).toHaveAttribute("aria-pressed", "true");
      await expect(botIssue).toHaveCount(0);
      await expect(humanIssue).toBeVisible();

      const settingsReset = page.waitForResponse(
        (response) => response.url().endsWith("/api/v1/settings") && response.request().method() === "PUT",
      );
      await hideBots.click();
      expect((await settingsReset).ok()).toBe(true);
      await expect(hideBots).toHaveAttribute("aria-pressed", "false");
      await expect(botIssue).toBeVisible();
    } finally {
      await server.stop();
    }
  });

  test("mobile PR and issue lists respect hide-org while preserving provider collisions", async ({ browser }) => {
    const server = await startIsolatedE2EServerWithOptions({ providerCollision: true });
    const page = await browser.newPage();
    try {
      await page.addInitScript(() => localStorage.setItem("kenn-forge:hideOrgName", "1"));

      await page.goto(`${server.info.base_url}/m/pulls`);
      await expect(page.locator(".pull-item .repo-name", { hasText: "github/github.com/acme/widgets" })).toHaveCount(4);
      await expect(page.locator(".pull-item .repo-name", { hasText: "gitea/github.com/acme/widgets" })).toHaveCount(1);
      await expect(page.locator(".pull-item .repo-name", { hasText: /^tools$/ }).first()).toHaveText("tools");

      await page.getByRole("combobox", { name: /Phone mode/ }).click();
      await page.getByRole("option", { name: "Issues" }).click();
      await expect(page.locator(".issue-item .repo-name", { hasText: "github/github.com/acme/widgets" })).toHaveCount(
        3,
      );
      await expect(page.locator(".issue-item .repo-name", { hasText: "gitea/github.com/acme/widgets" })).toHaveCount(1);
      await expect(page.locator(".issue-item .repo-name", { hasText: /^tools$/ }).first()).toHaveText("tools");
    } finally {
      await page.close();
      await server.stop();
    }
  });
});

test.describe("high-density phone routes", () => {
  const pixel7 = devices["Pixel 7"];
  test.use({
    viewport: pixel7.viewport,
    deviceScaleFactor: pixel7.deviceScaleFactor,
    userAgent: pixel7.userAgent,
  });

  test("mobile activity keeps the phone type scale and stays readable on high-density Android displays", async ({
    page,
  }) => {
    await page.goto("/m?range=30d&view=threaded");

    await expect(page.locator(".mobile-shell")).toBeVisible();
    await expect(page.locator(".mobile-activity-inbox")).toBeVisible();
    await page.getByRole("button", { name: /^Filters/ }).click();
    await expect(page.getByRole("switch", { name: "PRs" })).toBeVisible();
    await expect(page.getByRole("switch", { name: "Issues" })).toBeVisible();
    await page.getByRole("combobox", { name: /Time range/ }).click();
    await expect(page.getByRole("option", { name: "24h" })).toBeVisible();

    const metrics = await page.evaluate(() => {
      const fontSize = (selector: string): number => {
        const node = document.querySelector(selector);
        return node ? Number.parseFloat(getComputedStyle(node).fontSize) : 0;
      };
      const shell = document.querySelector(".mobile-shell");
      const inbox = document.querySelector(".mobile-activity-inbox");
      const tokenValue = (node: Element | null, name: string): string =>
        node ? getComputedStyle(node).getPropertyValue(name).trim() : "";
      const filterControls = [
        ...document.querySelectorAll(
          ".mobile-activity-filter-grid .kit-select-dropdown__trigger, .mobile-item-type-toggle .kit-toggle, .mobile-boolean-toggle .kit-toggle",
        ),
      ]
        .map((control) => control.getBoundingClientRect())
        .map((rect) => ({ left: rect.left, right: rect.right }));
      const search = document.querySelector(".kit-search-input")?.getBoundingClientRect();
      const firstOption = document
        .querySelector(".mobile-filter-dropdown .kit-select-dropdown__option")
        ?.getBoundingClientRect();
      return {
        dpr: window.devicePixelRatio,
        viewportWidth: window.innerWidth,
        documentWidth: document.documentElement.scrollWidth,
        mobileTypeToken: tokenValue(shell, "--mobile-type-body"),
        activityTypeToken: tokenValue(inbox, "--mobile-type-body"),
        densityScale: tokenValue(inbox, "--mobile-device-density-scale"),
        bodyFontSize: fontSize(".mobile-activity-inbox"),
        filterFontSize: fontSize(".mobile-item-type-toggle .kit-toggle__label"),
        filterOptionFontSize: fontSize(".mobile-filter-dropdown .kit-select-dropdown__option"),
        filterOptionHeight: firstOption?.height ?? 0,
        modePickerFontSize: fontSize(".mobile-mode-picker .kit-select-dropdown__trigger"),
        searchHeight: search?.height ?? 0,
        searchLeft: search?.left ?? 0,
        searchRight: search?.right ?? 0,
        filterControls,
      };
    });

    expect(metrics.dpr).toBeGreaterThanOrEqual(2.5);
    expect(metrics.mobileTypeToken).toBe("1rem");
    expect(metrics.activityTypeToken).toBe("1rem");
    expect(metrics.densityScale).toBe("");
    expect(metrics.documentWidth).toBeLessThanOrEqual(metrics.viewportWidth);
    expect(metrics.bodyFontSize).toBeGreaterThanOrEqual(16);
    expect(metrics.filterFontSize).toBeGreaterThanOrEqual(15);
    expect(metrics.filterOptionFontSize).toBeGreaterThanOrEqual(15);
    expect(metrics.filterOptionHeight).toBeGreaterThanOrEqual(44);
    expect(metrics.modePickerFontSize).toBeGreaterThanOrEqual(16);
    expect(metrics.searchHeight).toBeGreaterThanOrEqual(44);
    expect(metrics.searchLeft).toBeGreaterThanOrEqual(0);
    expect(metrics.searchRight).toBeLessThanOrEqual(metrics.viewportWidth);
    for (const control of metrics.filterControls) {
      expect(control.left).toBeGreaterThanOrEqual(0);
      expect(control.right).toBeLessThanOrEqual(metrics.viewportWidth);
    }
  });
});
