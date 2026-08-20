import { expect, test } from "@playwright/test";

/**
 * Tolerant smoke spec: asserts the app shell renders. The Go backend may be
 * up or down — without a session the router redirects to /login (which
 * renders a form); with a session the dashboard shell renders a nav rail.
 * Both paths must show the document title and a visible landmark.
 */
test.describe("app shell", () => {
  test("serves the page with the product title", async ({ page }) => {
    const response = await page.goto("/", { waitUntil: "domcontentloaded" });
    expect(response?.status(), "GET / should be served").toBeLessThan(500);
    await expect(page).toHaveTitle("Local Model Works");
  });

  test("mounts either the login form or the dashboard nav", async ({ page }) => {
    await page.goto("/", { waitUntil: "domcontentloaded" });
    const shell = page
      .locator("nav")
      .or(page.getByRole("form"))
      .first();
    await expect(shell, "login form or dashboard nav should render").toBeVisible({
      timeout: 15000,
    });
  });
});
