import Link from "next/link";
import { notFound } from "next/navigation";

import { Avatar, Badge, InteractiveCard } from "@/components/ui";
import { ApiError, formatMoney, getPublicProfile } from "@/lib/api";

/**
 * The public creator page, served at /@username.
 *
 * A dynamic segment rather than a literal "@" folder: in the App Router a
 * directory named @something is a parallel route slot, so the "@" has to live
 * in the value. The check below keeps this route from swallowing every
 * unmatched path — /dashboard resolves to its own static route first, and
 * anything that is not a handle is a 404 rather than a failed API call.
 */
function handleFrom(segment) {
  // The segment arrives percent-encoded on some code paths and decoded on
  // others — the same request reaches the page as "@rahul" and
  // generateMetadata as "%40rahul". Decoding first makes both spellings the
  // same value, which is why this is not simply segment.startsWith("@").
  let decoded = segment;
  try {
    decoded = decodeURIComponent(segment);
  } catch {
    // Malformed escapes (a bare "%") throw. Not a handle either way.
    return null;
  }

  return decoded.startsWith("@") ? decoded.slice(1) : null;
}

/**
 * No "use cache" here. The Go API already caches this page in Redis and drops
 * the entry when the creator edits it; caching again at this layer would add an
 * expiry that invalidation cannot reach, so an edit would look applied in the
 * database and stale on the site.
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
    // failure and belongs in the error boundary, not shown as an empty page.
    if (error instanceof ApiError && error.isNotFound) {
      notFound();
    }
    throw error;
  }

  const { creator, links, products } = page;

  return (
    <main className="relative mx-auto w-full max-w-lg px-5 pb-20 pt-16 sm:pt-24">
      {/* A soft wash behind the header. It gives the page a focal point without
          an image, and fades out before the content so nothing competes with
          the products. */}
      <div
        aria-hidden="true"
        className="pointer-events-none absolute inset-x-0 top-0 -z-10 h-72 bg-gradient-to-b from-brand-100/70 via-brand-50/30 to-transparent dark:from-brand-900/25 dark:via-brand-900/10"
      />

      <header className="animate-rise flex flex-col items-center text-center">
        <Avatar name={creator.display_name} size={84} />

        <h1 className="mt-5 text-[1.75rem] font-semibold leading-tight text-ink">
          {creator.display_name}
        </h1>
        <p className="mt-1 text-sm font-medium text-ink-muted">@{creator.username}</p>

        {creator.bio ? (
          <p className="mt-4 max-w-sm text-[0.9375rem] leading-relaxed text-ink-soft">
            {creator.bio}
          </p>
        ) : null}
      </header>

      {links.length > 0 ? (
        <nav aria-label="Links" className="stagger mt-10 flex flex-col gap-2.5">
          {links.map((link) => (
            <InteractiveCard key={link.click_url} as="a"
              // click_url, not url: it records the click and then redirects.
              // The destination stays in `url` so a visitor can see where a
              // link goes before following it, and a crawler can read it.
              href={link.click_url}
              // These leave our site: deny the tab-hijacking capability and keep
              // our URLs out of the destination's referrer log.
              rel="noopener noreferrer"
              className="flex items-center justify-between px-5 py-4"
            >
              <span className="text-[0.9375rem] font-medium text-ink">
                {link.title}
              </span>
              <ArrowIcon className="size-4 shrink-0 text-ink-muted transition-transform duration-250 ease-soft group-hover:translate-x-0.5 group-hover:text-brand-600" />
            </InteractiveCard>
          ))}
        </nav>
      ) : null}

      {products.length > 0 ? (
        <section aria-labelledby="products-heading" className="mt-12">
          <h2
            id="products-heading"
            className="mb-3 px-1 text-xs font-semibold uppercase tracking-[0.12em] text-ink-muted"
          >
            Products
          </h2>

          <ul className="stagger flex flex-col gap-2.5">
            {products.map((product) => (
              <li key={product.slug}>
                <InteractiveCard as={Link} href={`/${username}/${product.slug}`} className="block px-5 py-4">
                  <div className="flex items-start justify-between gap-4">
                    <span className="text-[0.9375rem] font-medium leading-snug text-ink transition-colors duration-250 ease-soft group-hover:text-brand-700 dark:group-hover:text-brand-300">
                      {product.title}
                    </span>
                    <Badge className="shrink-0 tabular-nums">
                      {formatMoney(product.price_minor, product.currency)}
                    </Badge>
                  </div>

                  {/* The description rather than a repeated "View details". The
                      card is already obviously a link; three identical calls to
                      action down the page are noise where the one line that
                      helps someone decide would go. Clamped to two lines so a
                      long description cannot reshape the list. */}
                  {product.description ? (
                    <p className="mt-1.5 line-clamp-2 text-sm leading-relaxed text-ink-muted">
                      {product.description}
                    </p>
                  ) : null}
                </InteractiveCard>
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      {links.length === 0 && products.length === 0 ? (
        <p className="animate-rise mt-14 text-center text-sm text-ink-muted">
          This page has nothing on it yet.
        </p>
      ) : null}

      <footer className="mt-16 text-center">
        <Link
          href="/"
          className="text-xs font-medium text-ink-muted transition-colors hover:text-ink-soft"
        >
          Made with CreatorFlow
        </Link>
      </footer>
    </main>
  );
}

function ArrowIcon({ className }) {
  return (
    <svg className={className} viewBox="0 0 16 16" fill="none" aria-hidden="true">
      <path
        d="M6 3.5 10.5 8 6 12.5"
        stroke="currentColor"
        strokeWidth="1.75"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
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
