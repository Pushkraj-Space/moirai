import { chromium } from "@playwright/test";
import assert from "node:assert/strict";
import { resolve } from "node:path";

const origin = process.env.MOIRAI_TEST_ORIGIN;
const browser = await chromium.launch({ headless: true });
try {
  const context = await browser.newContext();
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.goto(origin);
  await page
    .getByRole("heading", { name: "Move the session. Keep the thread." })
    .waitFor();
  await page
    .getByLabel("Email for product updates")
    .fill("browser-test@example.com");
  await page.getByRole("checkbox").check();
  await page.getByRole("button", { name: "Keep me posted" }).click();
  await page
    .getByText("You're subscribed. Thanks for following Moirai.")
    .waitFor();
  for (const width of [390, 1440]) {
    await page.setViewportSize({ width, height: 950 });
    assert(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
      `overflow at ${width}px`,
    );
    if (process.env.MOIRAI_SCREENSHOTS)
      await page.screenshot({
        path: resolve(process.env.MOIRAI_SCREENSHOTS, `landing-${width}.png`),
        fullPage: true,
      });
  }
  await context.addCookies([
    {
      name: "moirai_session",
      value: process.env.MOIRAI_TEST_TOKEN,
      url: origin,
      httpOnly: true,
      sameSite: "Lax",
    },
  ]);
  await page.goto(origin + "/app");
  await page
    .getByLabel("Portable .moirai archive")
    .setInputFiles(resolve("testdata/archive-v1.moirai"));
  await page.getByText("Prepared locally.", { exact: false }).waitFor();
  await page.getByRole("checkbox").check();
  await page.getByRole("button", { name: "Publish checkpoint" }).click();
  await page.waitForURL(/\/s\/[a-f0-9]{48}$/);
  const shareURL = page.url();
  await page.getByRole("heading", { name: "Checkpoint history" }).waitFor();
  if (process.env.MOIRAI_SCREENSHOTS)
    await page.screenshot({
      path: resolve(process.env.MOIRAI_SCREENSHOTS, "viewer.png"),
      fullPage: true,
    });
  const anonymous = await browser.newContext();
  assert.equal((await anonymous.request.get(shareURL)).status(), 404);
  const download = await context.request.get(
    shareURL.replace("/s/", "/v1/publications/") + "/archive",
  );
  assert.equal(download.status(), 200);
  assert.equal((await download.json()).format, "moirai.session");
  await page.goto(origin + "/app");
  page.on("dialog", (dialog) => dialog.accept());
  await page
    .getByRole("button", { name: "revoke", exact: true })
    .first()
    .click();
  await page.getByText("· revoked", { exact: false }).waitFor();
  assert.equal((await context.request.get(shareURL)).status(), 404);
  assert.deepEqual(errors, []);
  console.log(
    "Browser flow passed: responsive landing, waitlist, local review, private publication, viewer, download, revoke.",
  );
} finally {
  await browser.close();
}
