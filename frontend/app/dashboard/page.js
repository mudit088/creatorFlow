"use client";

import Link from "next/link";
import { useCallback, useEffect, useState } from "react";

import { useAuth } from "@/components/AuthProvider";
import CreateProfile from "@/components/CreateProfile";
import { Button, Card, Skeleton } from "@/components/ui";
import { ApiError, formatMoney } from "@/lib/api";

export default function OverviewPage() {
  const { authedRequest, status } = useAuth();

  const [profile, setProfile] = useState(null);
  const [overview, setOverview] = useState(null);
  const [state, setState] = useState("loading"); // loading | no-profile | ready | failed

  const load = useCallback(async () => {
    try {
      const me = await authedRequest("/api/v1/profiles/me");
      setProfile(me);
    } catch (error) {
      // 404 here is not a failure — it is a creator who has signed up but not
      // claimed a handle yet, which is the single most common state for a new
      // account and deserves a first-run screen rather than an error.
      if (error instanceof ApiError && error.isNotFound) {
        setState("no-profile");
        return;
      }
      setState("failed");
      return;
    }

    try {
      const stats = await authedRequest("/api/v1/analytics/overview");
      setOverview(stats);
    } catch {
      // Analytics is the least important thing on this page. If it fails, the
      // profile summary below is still worth showing.
      setOverview(null);
    }
    setState("ready");
  }, [authedRequest]);

  useEffect(() => {
    if (status !== "authenticated") return undefined;
    const timer = setTimeout(load, 0);
    return () => clearTimeout(timer);
  }, [status, load]);

  if (state === "loading") {
    return (
      <div>
        <Skeleton className="h-8 w-48" />
        <div className="mt-6 grid grid-cols-2 gap-3 sm:grid-cols-4">
          {[0, 1, 2, 3].map((i) => (
            <Skeleton key={i} className="h-24 rounded-card" />
          ))}
        </div>
        <Skeleton className="mt-6 h-40 w-full rounded-card" />
      </div>
    );
  }

  if (state === "no-profile") {
    return <CreateProfile onCreated={load} />;
  }

  if (state === "failed") {
    return (
      <Card className="p-6 text-center">
        <p className="text-sm text-ink-soft">Could not load your dashboard.</p>
        <Button variant="secondary" size="sm" className="mt-4" onClick={load}>
          Try again
        </Button>
      </Card>
    );
  }

  const totals = overview?.totals;
  const revenue = overview?.revenue;

  return (
    <div className="animate-rise">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold text-ink">{profile.display_name}</h1>
          <p className="mt-1 text-sm text-ink-muted">
            {profile.is_published ? (
              <>
                Live at{" "}
                <Link
                  href={`/@${profile.username}`}
                  className="font-medium text-brand-600 underline-offset-4 hover:underline dark:text-brand-300"
                >
                  /@{profile.username}
                </Link>
              </>
            ) : (
              <>Your page is a draft — nobody can see it yet.</>
            )}
          </p>
        </div>
        <Button as={Link} href="/dashboard/page-editor" variant="secondary" size="sm">
          Edit page
        </Button>
      </div>

      {/* Last 30 days, which is what the API defaults to. Labelled, because a
          number without a period is a number nobody can act on. */}
      <section aria-label="Last 30 days" className="mt-6">
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
          <Stat label="Visitors" value={totals?.daily_visitor_total ?? 0} />
          <Stat label="Page views" value={totals?.profile_views ?? 0} />
          <Stat label="Sales" value={revenue?.paid_orders ?? 0} />
          <Stat
            label="Revenue"
            value={formatMoney(revenue?.revenue_minor ?? 0, revenue?.currency || "INR")}
          />
        </div>
        <p className="mt-2 px-1 text-xs text-ink-muted">
          Last 30 days. Visitors counts each day separately, so someone who
          returns on three days counts three times.
        </p>
      </section>

      {overview?.top_products?.length ? (
        <section className="mt-8">
          <h2 className="mb-3 text-xs font-semibold uppercase tracking-[0.12em] text-ink-muted">
            Top products
          </h2>
          <Card className="divide-y divide-line">
            {overview.top_products.map((product) => (
              <div key={product.product_id} className="flex items-center gap-4 px-4 py-3">
                <span className="min-w-0 flex-1 truncate text-sm font-medium text-ink">
                  {product.title}
                </span>
                <span className="shrink-0 text-xs tabular-nums text-ink-muted">
                  {product.views} views
                </span>
                <span className="shrink-0 text-xs font-medium tabular-nums text-ink">
                  {product.purchases} sold
                </span>
              </div>
            ))}
          </Card>
        </section>
      ) : (
        <Card className="mt-8 p-6 text-center">
          <p className="text-sm text-ink-soft">
            No sales yet. Add a product and share your page to get started.
          </p>
          <Button as={Link} href="/dashboard/products" size="sm" className="mt-4">
            Add a product
          </Button>
        </Card>
      )}
    </div>
  );
}

function Stat({ label, value }) {
  return (
    <Card className="px-4 py-3.5">
      <p className="text-xs font-medium text-ink-muted">{label}</p>
      {/* tabular-nums so a column of figures lines up and does not jitter as
          values change. */}
      <p className="mt-1 text-xl font-semibold tabular-nums text-ink">{value}</p>
    </Card>
  );
}
