import Link from "next/link";
import { notFound } from "next/navigation";

import { ApiError, formatMoney, getPublicProfile } from "@/lib/api";

/**
 * The public creator page, served at /@username.
 *
 * A dynamic segment rather than a literal "@" folder: in the App Router a
 * directory named @something is a parallel route slot, so the "@" has to live
 * in the value. The check below is what keeps this route from swallowing every
 * unmatched path — /dashboard resolves to its own static route first, and
 * anything else that is not a handle is a 404 rather than a failed API call.
 */
function handleFrom(segment) {
  // The segment arrives percent-encoded on some code paths and decoded on
  // others — the same request reaches the page as "@rahul" and generateMetadata
  // as "%40rahul". Decoding first makes both spellings the same value, which is
  // why this is not simply segment.startsWith("@").
  let decoded = segment;
  try {
    decoded = decodeURIComponent(segment);
  } catch {
    // Malformed escapes (a bare "%") throw. That is not a handle either way.
    return null;
  }

  return decoded.startsWith("@") ? decoded.slice(1) : null;
}

/**
 * Note there is no "use cache" here. The Go API already caches this page in
 * Redis and drops the entry the moment the creator edits it; caching again at
 * this layer would add an expiry that invalidation cannot reach, so an edit
 * would look applied in the database and stale on the site.
 */
export default async function CreatorPage({ params }) {
  // params is a Promise in Next 16 — awaiting it is required, not stylistic.
  const { username } = await params;

  const handle = handleFrom(username);
  if (!handle) {
    notFound();
  }

  let page;
  try {
    page = await getPublicProfile(handle);
  } catch (error) {
    // The API answers 404 for "no such creator", "not published yet" and
    // "reserved handle" identically, on purpose — distinguishing them would
    // turn this page into a directory of drafts. Anything else is a real
    // failure and should reach the error boundary rather than be shown as an
    // empty page.
    if (error instanceof ApiError && error.isNotFound) {
      notFound();
    }
    throw error;
  }

  const { creator, links, products } = page;

  return (
    <main className="mx-auto min-h-screen w-full max-w-xl px-5 py-14">
      <header className="text-center">
        <h1 className="text-2xl font-semibold tracking-tight text-zinc-900 dark:text-zinc-50">
          {creator.display_name}
        </h1>
        <p className="mt-1 text-sm text-zinc-500 dark:text-zinc-400">@{creator.username}</p>
        {creator.bio ? (
          <p className="mx-auto mt-4 max-w-md text-balance text-sm leading-relaxed text-zinc-600 dark:text-zinc-300">
            {creator.bio}
          </p>
        ) : null}
      </header>

      {links.length > 0 ? (
        <nav className="mt-10 flex flex-col gap-3">
          {links.map((link) => (
            <a
              key={link.click_url}
              // click_url, not url: it records the click and then redirects.
              // The destination is still in `url` so a visitor can see where a
              // link goes before following it, and so a crawler can read it.
              href={link.click_url}
              className="rounded-xl border border-zinc-200 bg-white px-4 py-3 text-center text-sm font-medium text-zinc-800 transition hover:border-zinc-300 hover:bg-zinc-50 dark:border-zinc-800 dark:bg-zinc-900 dark:text-zinc-100 dark:hover:bg-zinc-800"
              // These leave our site, so deny the tab-hijacking capability and
              // keep our URLs out of the destination's referrer log.
              rel="noopener noreferrer"
            >
              {link.title}
            </a>
          ))}
        </nav>
      ) : null}

      {products.length > 0 ? (
        <section className="mt-12">
          <h2 className="mb-4 text-xs font-semibold uppercase tracking-widest text-zinc-400">
            Products
          </h2>
          <ul className="flex flex-col gap-3">
            {products.map((product) => (
              <li key={product.slug}>
                <Link
                  href={`/${username}/${product.slug}`}
                  className="flex items-center justify-between gap-4 rounded-xl border border-zinc-200 bg-white px-4 py-4 transition hover:border-zinc-300 hover:bg-zinc-50 dark:border-zinc-800 dark:bg-zinc-900 dark:hover:bg-zinc-800"
                >
                  <span className="text-sm font-medium text-zinc-900 dark:text-zinc-50">
                    {product.title}
                  </span>
                  <span className="shrink-0 text-sm tabular-nums text-zinc-600 dark:text-zinc-300">
                    {formatMoney(product.price_minor, product.currency)}
                  </span>
                </Link>
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      {links.length === 0 && products.length === 0 ? (
        <p className="mt-12 text-center text-sm text-zinc-500">
          This page has nothing on it yet.
        </p>
      ) : null}
    </main>
  );
}

/**
 * Metadata is fetched separately, which looks like a duplicate request but is
 * not: Next deduplicates identical fetches within one render pass, so this and
 * the page body share a single call to the API.
 */
export async function generateMetadata({ params }) {
  const { username } = await params;
  const handle = handleFrom(username);
  if (!handle) {
    return {};
  }

  try {
    const { creator } = await getPublicProfile(handle);
    return {
      title: `${creator.display_name} (@${creator.username})`,
      description: creator.bio || `Links and products from @${creator.username}.`,
      openGraph: {
        title: `${creator.display_name} (@${creator.username})`,
        description: creator.bio || undefined,
        type: "profile",
      },
    };
  } catch {
    // A missing creator must not make the page throw before notFound() runs.
    return {};
  }
}
