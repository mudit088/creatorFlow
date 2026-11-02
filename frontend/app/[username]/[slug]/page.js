import Link from "next/link";
import { notFound } from "next/navigation";

import BuyPanel from "@/components/BuyPanel";
import { Avatar, Card } from "@/components/ui";
import { ApiError, formatMoney, getPublicProduct } from "@/lib/api";

function handleFrom(segment) {
  let decoded = segment;
  try {
    decoded = decodeURIComponent(segment);
  } catch {
    return null;
  }
  return decoded.startsWith("@") ? decoded.slice(1) : null;
}

/** Bytes to something a person reads. */
function formatSize(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

export default async function ProductPage({ params }) {
  const { username, slug } = await params;

  const handle = handleFrom(username);
  if (!handle) {
    notFound();
  }

  let product;
  try {
    product = await getPublicProduct(handle, slug);
  } catch (error) {
    if (error instanceof ApiError && error.isNotFound) {
      notFound();
    }
    throw error;
  }

  const { creator, includes } = product;

  return (
    <main className="relative mx-auto w-full max-w-lg px-5 pb-24 pt-8">
      <div
        aria-hidden="true"
        className="pointer-events-none absolute inset-x-0 top-0 -z-10 h-64 bg-gradient-to-b from-brand-100/60 via-brand-50/20 to-transparent dark:from-brand-900/20 dark:via-brand-900/5"
      />

      {/* Back to the creator, not browser history: someone arriving from a
          shared link has no history to go back to, and a dead-ended page is
          how a storefront loses the rest of its catalogue. */}
      <Link
        href={`/${username}`}
        className="group inline-flex items-center gap-2 text-sm font-medium text-ink-muted transition-colors hover:text-ink"
      >
        <svg className="size-4 transition-transform duration-250 ease-soft group-hover:-translate-x-0.5" viewBox="0 0 16 16" fill="none" aria-hidden="true">
          <path d="M10 3.5 5.5 8l4.5 4.5" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
        {creator.display_name}
      </Link>

      <article className="animate-rise mt-6">
        <h1 className="text-[2rem] font-semibold leading-[1.15] text-ink">{product.title}</h1>

        <div className="mt-3 flex items-center gap-2.5">
          <Avatar name={creator.display_name} size={28} />
          <span className="text-sm text-ink-soft">
            by <span className="font-medium text-ink">{creator.display_name}</span>
          </span>
        </div>

        {product.description ? (
          <p className="mt-6 whitespace-pre-line text-[0.9375rem] leading-relaxed text-ink-soft">
            {product.description}
          </p>
        ) : null}

        {includes.length > 0 ? (
          <section aria-labelledby="includes-heading" className="mt-8">
            <h2 id="includes-heading" className="mb-3 text-xs font-semibold uppercase tracking-[0.12em] text-ink-muted">
              What you get
            </h2>
            <Card className="divide-y divide-line overflow-hidden">
              {includes.map((file) => (
                <div key={file.name} className="flex items-center gap-3 px-4 py-3">
                  <FileIcon className="size-4 shrink-0 text-ink-muted" />
                  <span className="min-w-0 flex-1 truncate text-sm text-ink">{file.name}</span>
                  <span className="shrink-0 text-xs tabular-nums text-ink-muted">
                    {formatSize(file.size_bytes)}
                  </span>
                </div>
              ))}
            </Card>
          </section>
        ) : null}
      </article>

      {/* The purchase itself is a client component: it holds form state, talks
          to the API and opens the payment window. Everything above stays a
          server component, so the page renders and is indexable before any
          JavaScript loads. */}
      <BuyPanel
        productId={product.id}
        priceLabel={formatMoney(product.price_minor, product.currency)}
        productTitle={product.title}
        creatorName={creator.display_name}
      />
    </main>
  );
}

function FileIcon({ className }) {
  return (
    <svg className={className} viewBox="0 0 16 16" fill="none" aria-hidden="true">
      <path
        d="M9 1.5H4.5a1 1 0 0 0-1 1v11a1 1 0 0 0 1 1h7a1 1 0 0 0 1-1V5L9 1.5Z"
        stroke="currentColor"
        strokeWidth="1.3"
        strokeLinejoin="round"
      />
      <path d="M9 1.5V5h3.5" stroke="currentColor" strokeWidth="1.3" strokeLinejoin="round" />
    </svg>
  );
}

export async function generateMetadata({ params }) {
  const { username, slug } = await params;
  const handle = handleFrom(username);
  if (!handle) {
    return {};
  }

  try {
    const product = await getPublicProduct(handle, slug);
    return {
      title: product.title,
      description: product.description || `${product.title} by ${product.creator.display_name}.`,
      openGraph: {
        title: product.title,
        description: product.description || undefined,
        type: "website",
      },
    };
  } catch {
    return {};
  }
}
