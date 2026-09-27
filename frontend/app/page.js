import Link from "next/link";

/**
 * The root page exists mainly so that "/" is not a 404 while the marketing site
 * does not exist. It is deliberately static: no API call, nothing to cache, and
 * nothing that breaks when the backend is down.
 */
export default function Home() {
  return (
    <main className="mx-auto flex min-h-screen w-full max-w-xl flex-col justify-center px-5 py-14">
      <h1 className="text-3xl font-semibold tracking-tight">CreatorFlow</h1>
      <p className="mt-3 text-sm leading-relaxed text-zinc-600 dark:text-zinc-300">
        One page for your links and your digital products. Buyers pay, and the
        files are delivered automatically.
      </p>
      <p className="mt-8 text-sm text-zinc-500">
        Creator pages live at{" "}
        <code className="rounded bg-zinc-200 px-1.5 py-0.5 text-xs dark:bg-zinc-800">
          /@username
        </code>
        .
      </p>
      <Link
        href="/dashboard"
        className="mt-6 w-fit rounded-lg bg-zinc-900 px-4 py-2 text-sm font-medium text-white transition hover:bg-zinc-700 dark:bg-zinc-50 dark:text-zinc-900 dark:hover:bg-zinc-200"
      >
        Creator dashboard
      </Link>
    </main>
  );
}
