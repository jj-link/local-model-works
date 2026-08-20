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

  test("mounts a visible application landmark", async ({ page }) => {
    await page.route("**/api/v1/session", (route) =>
      route.fulfill({ status: 401, contentType: "application/json", body: "{\"code\":\"auth.unauthorized\"}" }),
    );
    await page.goto("/", { waitUntil: "domcontentloaded" });
    await expect(page.locator("main").first()).toBeVisible({ timeout: 15000 });
  });
});
