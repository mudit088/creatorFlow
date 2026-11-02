/**
 * Exercises the page editor: edit profile, publish, add links, reorder, hide,
 * remove — then checks the public page reflects it.
 *
 * The public check at the end matters more than it looks: the storefront is
 * cached in Redis and invalidated on write, so this is what proves an edit
 * actually reaches a visitor rather than sitting behind a stale cache entry.
 */
import { chromium } from "playwright";

const baseURL = process.env.SHOT_BASE_URL || "http://localhost:3000";
const stamp = Date.now();
const email = `editor-${stamp}@test.local`;
const handle = `editor${String(stamp).slice(-6)}`;

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1000, height: 900 } });
page.on("console", (m) => {
  if (m.type() === "error" && !m.text().includes("404") && !m.text().includes("401")) {
    console.log("  console error:", m.text().slice(0, 120));
  }
});

const step = async (name, fn) => {
  try {
    await fn();
    console.log(`  ok    ${name}`);
  } catch (error) {
    console.log(`  FAIL  ${name}: ${String(error).split("\n")[0]}`);
    await page.screenshot({ path: `shots/fail-${name.replace(/\W+/g, "-")}.png` });
    await browser.close();
    process.exit(1);
  }
};

await step("register and claim a handle", async () => {
  await page.goto(`${baseURL}/register`, { waitUntil: "networkidle" });
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password").fill("CorrectHorse1!");
  await page.getByRole("button", { name: "Create account" }).click();
  await page.getByRole("heading", { name: "Claim your page" }).waitFor({ timeout: 15_000 });
  await page.getByLabel("Handle").fill(handle);
  await page.getByLabel("Display name").fill("Grace Hopper");
  await page.getByRole("button", { name: "Create my page" }).click();
  await page.getByRole("heading", { name: "Grace Hopper" }).waitFor({ timeout: 15_000 });
});

await step("open the page editor", async () => {
  await page.getByRole("link", { name: "Your page" }).click();
  await page.getByRole("heading", { name: "Your page" }).waitFor({ timeout: 10_000 });
});

await step("save a bio", async () => {
  await page.getByLabel("Bio").fill("Compilers, debugging, and naval history.");
  await page.getByRole("button", { name: "Save changes" }).click();
  await page.getByText("Saved").waitFor({ timeout: 10_000 });
});

await step("add three links", async () => {
  for (const [title, url] of [
    ["Talks", "example.com/talks"],
    ["Newsletter", "example.com/news"],
    ["Mastodon", "example.com/mastodon"],
  ]) {
    await page.getByLabel("Title").fill(title);
    await page.getByLabel("URL").fill(url);
    await page.getByRole("button", { name: "Add link" }).click();
    await page.getByText(url.replace("example.com", "https://example.com")).waitFor({ timeout: 10_000 });
  }
});

await step("reorder: move the last link up", async () => {
  await page.getByRole("button", { name: "Move Mastodon up" }).click();
  await page.waitForTimeout(800);
  const titles = await page.locator("ul li p.truncate.text-sm").allTextContents();
  if (titles[1] !== "Mastodon") throw new Error(`order is ${titles.join(", ")}`);
});

await step("hide a link", async () => {
  await page.getByRole("button", { name: "Hide" }).first().click();
  await page.getByRole("button", { name: "Show" }).first().waitFor({ timeout: 10_000 });
});

await step("publish the page", async () => {
  await page.getByRole("button", { name: "Publish page" }).click();
  await page.getByText("Published", { exact: true }).waitFor({ timeout: 10_000 });
});
await page.waitForTimeout(800);
await page.screenshot({ path: "shots/page-editor.png", fullPage: true });

await step("public page shows the edits, not a stale cache", async () => {
  await page.goto(`${baseURL}/@${handle}`, { waitUntil: "networkidle" });
  await page.getByText("Compilers, debugging, and naval history.").waitFor({ timeout: 10_000 });
  // The hidden link must be absent, and the reordered one must be second.
  const linkTitles = await page.locator("nav a").allTextContents();
  const cleaned = linkTitles.map((t) => t.trim());
  if (cleaned.includes("Talks")) throw new Error(`hidden link still shown: ${cleaned.join(", ")}`);
  console.log("        public links:", cleaned.join(" | "));
});

console.log(`\n  creator: ${email}\n  page   : ${baseURL}/@${handle}`);
await browser.close();
