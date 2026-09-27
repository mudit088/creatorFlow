import Link from "next/link";

/**
 * Shown for an unknown creator, an unpublished page, and a product that does
 * not exist — all of which the API reports identically so that this page cannot
 * be used to discover which handles are taken.
 */
export default function NotFound() {
  return (
    <main className="mx-auto flex min-h-screen w-full max-w-xl flex-col justify-center px-5 text-center">
      <h1 className="text-2xl font-semibold tracking-tight">Nothing here</h1>
      <p className="mt-3 text-sm text-zinc-600 dark:text-zinc-300">
        This page does not exist, or it has not been published yet.
      </p>
      <Link href="/" className="mt-6 text-sm text-zinc-500 underline underline-offset-4 hover:text-zinc-800 dark:hover:text-zinc-200">
        Back to the start
      </Link>
    </main>
  );
}
