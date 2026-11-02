/**
 * Walks the creator journey in a real browser: register, claim a handle, land
 * on the dashboard, reload to prove the session survives, and sign out.
 *
 * The reload is the point. The access token lives in memory only, so a reload
 * destroys it — and the session has to be rebuilt from the HttpOnly refresh
 * cookie. That is exactly the kind of thing that works in development until
 * someone presses F5.
 */
import { chromium } from "playwright";

const baseURL = process.env.SHOT_BASE_URL || "http://localhost:3000";
const stamp = Date.now();
const email = `creator-${stamp}@test.local`;
const handle = `creator${String(stamp).slice(-6)}`;

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1000, height: 820 } });

page.on("console", (msg) => {
  if (msg.type() === "error") console.log("  console error:", msg.text().slice(0, 140));
});

// Entrance animations run for ~500ms. Screenshotting the moment an element
// appears captures it mid-fade, which looks like a rendering bug in review.
const settle = () => page.waitForTimeout(800);

const step = async (name, fn) => {
  try {
    await fn();
    console.log(`  ok    ${name}`);
  } catch (error) {
    console.log(`  FAIL  ${name}: ${String(error).split("\n")[0]}`);
    await page.screenshot({ path: `shots/fail-${name.replace(/\W+/g, "-")}.png` });
    throw error;
  }
};

await step("protected route redirects when signed out", async () => {
  await page.goto(`${baseURL}/dashboard`, { waitUntil: "networkidle" });
  await page.waitForURL(/\/login\?next=/, { timeout: 10_000 });
});

await step("register", async () => {
  await page.goto(`${baseURL}/register`, { waitUntil: "networkidle" });
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password").fill("CorrectHorse1!");
  await page.getByRole("button", { name: "Create account" }).click();
  await page.waitForURL(/\/dashboard/, { timeout: 15_000 });
});

await step("first run asks for a handle", async () => {
  await page.getByRole("heading", { name: "Claim your page" }).waitFor({ timeout: 10_000 });
});
await settle();
await page.screenshot({ path: "shots/first-run.png" });

await step("claim the handle", async () => {
  await page.getByLabel("Handle").fill(handle);
  await page.getByLabel("Display name").fill("Ada Lovelace");
  await page.getByRole("button", { name: "Create my page" }).click();
  await page.getByRole("heading", { name: "Ada Lovelace" }).waitFor({ timeout: 15_000 });
});
await settle();
await page.screenshot({ path: "shots/dashboard.png" });

await step("session survives a reload", async () => {
  await page.reload({ waitUntil: "networkidle" });
  await page.getByRole("heading", { name: "Ada Lovelace" }).waitFor({ timeout: 15_000 });
  if (page.url().includes("/login")) throw new Error("bounced to /login after reload");
});

await step("sign out", async () => {
  await page.getByRole("button", { name: "Sign out" }).click();
  await page.waitForURL(/\/login/, { timeout: 10_000 });
});

await step("signed out stays out", async () => {
  await page.goto(`${baseURL}/dashboard`, { waitUntil: "networkidle" });
  await page.waitForURL(/\/login/, { timeout: 10_000 });
});

console.log(`\n  creator: ${email}\n  handle : @${handle}`);
await browser.close();
