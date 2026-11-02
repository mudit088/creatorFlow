/**
 * Screenshots pages so design work can be looked at rather than guessed at.
 *
 * Run: node scripts/shoot.mjs <name> <path> [--width=N] [--dark]
 *
 * Kept in the repo rather than a scratch file because reviewing a visual change
 * is part of making one, and the next person should not have to rebuild this.
 */
import { chromium } from "playwright";
import { mkdir } from "node:fs/promises";

const [name, path = "/"] = process.argv.slice(2);
const args = process.argv.slice(2);

const widthArg = args.find((a) => a.startsWith("--width="));
const width = widthArg ? Number(widthArg.split("=")[1]) : 1280;
const dark = args.includes("--dark");
const fullPage = !args.includes("--viewport");

const baseURL = process.env.SHOT_BASE_URL || "http://localhost:3000";
const outDir = process.env.SHOT_DIR || "shots";

await mkdir(outDir, { recursive: true });

const browser = await chromium.launch();
const context = await browser.newContext({
  viewport: { width, height: 900 },
  // Device scale 2 so text renders the way it does on a modern display; at 1x
  // antialiasing differences make typography look worse than it is.
  deviceScaleFactor: 2,
  colorScheme: dark ? "dark" : "light",
});

const page = await context.newPage();
const response = await page.goto(`${baseURL}${path}`, { waitUntil: "networkidle" });

// Entrance animations are the point of the design; waiting for them to settle
// means the screenshot shows the resting state rather than a frame mid-fade.
await page.waitForTimeout(900);

const file = `${outDir}/${name}.png`;
await page.screenshot({ path: file, fullPage });

console.log(`${file}  ${response?.status()}  ${width}px  ${dark ? "dark" : "light"}`);

await browser.close();
