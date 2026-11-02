"use client";

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useEffect } from "react";

import { useAuth } from "@/components/AuthProvider";
import { Button, Skeleton, cx } from "@/components/ui";

const NAV = [
  { href: "/dashboard", label: "Overview" },
  { href: "/dashboard/page-editor", label: "Your page" },
  { href: "/dashboard/products", label: "Products" },
];

/**
 * The signed-in frame: guards the route, then renders navigation around it.
 *
 * The guard is client-side because the session lives in memory and is rebuilt
 * from an HttpOnly cookie after load — the server has no way to know who this
 * is at render time. That is fine for *navigation*, and it is explicitly not
 * the security boundary: every endpoint behind this verifies the access token
 * itself, so a visitor who skips the redirect sees an empty shell and a
 * sequence of 401s rather than anyone's data.
 */
export default function DashboardShell({ children }) {
  const { status, user, logout } = useAuth();
  const router = useRouter();
  const pathname = usePathname();

  useEffect(() => {
    if (status === "anonymous") {
      // `next` brings them back to the page they wanted after signing in.
      router.replace(`/login?next=${encodeURIComponent(pathname)}`);
    }
  }, [status, router, pathname]);

  if (status !== "authenticated") {
    // A skeleton rather than a spinner or a redirect flash: the session is
    // usually restored within a few hundred milliseconds, and showing the
    // shape of what is coming is calmer than a flicker of "signed out".
    return (
      <div className="mx-auto w-full max-w-3xl px-5 py-10">
        <Skeleton className="h-9 w-40" />
        <Skeleton className="mt-6 h-32 w-full rounded-card" />
        <Skeleton className="mt-3 h-32 w-full rounded-card" />
      </div>
    );
  }

  return (
    <div className="min-h-screen">
      <header className="sticky top-0 z-20 border-b border-line bg-canvas/85 backdrop-blur-md">
        <div className="mx-auto flex h-14 w-full max-w-3xl items-center gap-4 px-5">
          <Link href="/dashboard" className="text-sm font-semibold tracking-tight text-ink">
            CreatorFlow
          </Link>

          <nav className="flex flex-1 items-center gap-1" aria-label="Dashboard">
            {NAV.map((item) => {
              // Exact match for the root so "Overview" is not permanently
              // highlighted on every nested page.
              const active =
                item.href === "/dashboard" ? pathname === item.href : pathname.startsWith(item.href);
              return (
                <Link
                  key={item.href}
                  href={item.href}
                  aria-current={active ? "page" : undefined}
                  className={cx(
                    "rounded-control px-3 py-1.5 text-sm font-medium transition-colors duration-200",
                    active
                      ? "bg-brand-50 text-brand-700 dark:bg-brand-900/40 dark:text-brand-200"
                      : "text-ink-soft hover:bg-card hover:text-ink",
                  )}
                >
                  {item.label}
                </Link>
              );
            })}
          </nav>

          <span className="hidden max-w-[12rem] truncate text-xs text-ink-muted sm:block">
            {user?.email}
          </span>
          <Button variant="ghost" size="sm" onClick={() => logout().then(() => router.replace("/login"))}>
            Sign out
          </Button>
        </div>
      </header>

      <main className="mx-auto w-full max-w-3xl px-5 py-8">{children}</main>
    </div>
  );
}
