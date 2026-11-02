/**
 * The creator's product flow, in a browser: create a product, try to publish it
 * with nothing to deliver, upload a real file straight to storage, publish, and
 * confirm it appears on the public page and can be bought.
 *
 * The refused publish is the interesting step. It is a rule the API enforces
 * across two tables, and the only way to know the UI surfaces it usefully is to
 * trigger it.
 */
import { chromium } from "playwright";
import path from "node:path";

const baseURL = process.env.SHOT_BASE_URL || "http://localhost:3000";
const stamp = Date.now();
const email = `products-${stamp}@test.local`;
const handle = `maker${String(stamp).slice(-6)}`;
const samplePdf = path.resolve("../docs/sample-1024.pdf");

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1000, height: 1000 } });
page.on("console", (m) => {
  if (m.type() === "error" && !/40[14]/.test(m.text())) {
    console.log("  console error:", m.text().slice(0, 120));
  }
});

const step = async (name, fn) => {
  try {
    await fn();
    console.log(`  ok    ${name}`);
  } catch (error) {
    console.log(`  FAIL  ${name}: ${String(error).split("\n")[0]}`);
    await page.screenshot({ path: `shots/fail-${name.replace(/\W+/g, "-")}.png`, fullPage: true });
    await browser.close();
    process.exit(1);
  }
};

await step("set up a published creator", async () => {
  await page.goto(`${baseURL}/register`, { waitUntil: "networkidle" });
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password").fill("CorrectHorse1!");
  await page.getByRole("button", { name: "Create account" }).click();
  await page.getByRole("heading", { name: "Claim your page" }).waitFor({ timeout: 15_000 });
  await page.getByLabel("Handle").fill(handle);
  await page.getByLabel("Display name").fill("Alan Turing");
  await page.getByRole("button", { name: "Create my page" }).click();
  await page.getByRole("heading", { name: "Alan Turing" }).waitFor({ timeout: 15_000 });

  await page.getByRole("link", { name: "Your page" }).click();
  await page.getByRole("button", { name: "Publish page" }).click();
  await page.getByText("Published", { exact: true }).waitFor({ timeout: 10_000 });
});

await step("empty state invites a first product", async () => {
  await page.getByRole("link", { name: "Products" }).click();
  await page.getByText("Nothing for sale yet").waitFor({ timeout: 10_000 });
});

await step("create a product", async () => {
  await page.getByRole("button", { name: "Create your first product" }).click();
  await page.getByLabel("Title").fill("On Computable Numbers");
  await page.getByLabel("Price").fill("249");
  await page.getByLabel("Description").fill("The 1936 paper, typeset for reading.");
  await page.getByRole("button", { name: "Create product" }).click();
  await page.getByRole("heading", { name: "On Computable Numbers" }).waitFor({ timeout: 15_000 });
});

await step("publishing with no file is refused, with a useful reason", async () => {
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await page.getByText(/Upload a file before publishing/).waitFor({ timeout: 10_000 });
});
await page.waitForTimeout(500);
await page.screenshot({ path: "shots/products-refused.png", fullPage: true });

await step("upload a file straight to storage", async () => {
  await page.getByRole("button", { name: "Files" }).click();
  await page.getByText("No files yet.").waitFor({ timeout: 10_000 });
  await page.setInputFiles("input[type=file]", samplePdf);
  // Wait for the confirmed file row, not the progress label — both contain the
  // filename, and the progress one appears first.
  await page.getByRole("button", { name: "Remove" }).waitFor({ timeout: 25_000 });
  await page.getByText("Uploading").waitFor({ state: "detached", timeout: 25_000 });
});

await step("publish now that there is something to deliver", async () => {
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await page.getByText("Live", { exact: true }).waitFor({ timeout: 15_000 });
});
await page.waitForTimeout(600);
await page.screenshot({ path: "shots/products.png", fullPage: true });

await step("the product is live on the public page", async () => {
  await page.goto(`${baseURL}/@${handle}`, { waitUntil: "networkidle" });
  await page.getByText("On Computable Numbers").waitFor({ timeout: 10_000 });
  await page.getByText("₹249.00").waitFor({ timeout: 10_000 });
});

await step("and it can be bought", async () => {
  await page.getByText("On Computable Numbers").click();
  await page.getByRole("button", { name: /^Buy for/ }).waitFor({ timeout: 10_000 });
  await page.getByText("sample-1024.pdf").waitFor({ timeout: 10_000 });
});

console.log(`\n  creator: ${email}\n  page   : ${baseURL}/@${handle}`);
await browser.close();
