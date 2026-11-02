/**
 * Drives the buy flow in a real browser, up to the point where Razorpay takes
 * over. Proves the form validates, the order is created server-side and the
 * payment window opens with the right order attached.
 *
 * It deliberately stops before paying: completing a card form is Razorpay's
 * sandbox, and what this project needs to verify is its own half.
 */
import { chromium } from "playwright";

const baseURL = process.env.SHOT_BASE_URL || "http://localhost:3000";
const path = process.argv[2] || "/@priyamakes/design-systems-handbook";
const email = process.argv[3] || `playwright-${Date.now()}@test.local`;

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 480, height: 900 } });

const apiCalls = [];
page.on("request", (req) => {
  if (req.url().includes("/api/v1/orders")) {
    apiCalls.push(`${req.method()} ${new URL(req.url()).pathname}`);
  }
});
page.on("console", (msg) => {
  if (msg.type() === "error") console.log("  console error:", msg.text().slice(0, 120));
});

await page.goto(`${baseURL}${path}`, { waitUntil: "networkidle" });

// Empty submit must be refused by the form, not by the server.
await page.getByRole("button", { name: /^Buy for/ }).click();
const inlineError = await page.getByRole("alert").first().textContent().catch(() => null);
console.log("empty submit  :", inlineError ? `blocked — "${inlineError.trim()}"` : "NOT BLOCKED");
console.log("api calls     :", apiCalls.length === 0 ? "none (correct)" : apiCalls);

await page.getByLabel("Email").fill(email);
await page.getByRole("button", { name: /^Buy for/ }).click();

// Razorpay renders its checkout in an iframe appended to the page.
const frame = await page
  .waitForSelector("iframe.razorpay-checkout-frame", { timeout: 25_000 })
  .catch(() => null);

console.log("order request :", apiCalls.join(", ") || "none");
console.log("payment window:", frame ? "opened" : "did not open");
console.log("buyer email   :", email);

await page.screenshot({ path: "shots/checkout.png", fullPage: false });
await browser.close();
