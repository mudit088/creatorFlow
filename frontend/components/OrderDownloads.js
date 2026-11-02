"use client";

import { useCallback, useEffect, useRef, useState } from "react";

import { Button, Card, Field, Input, Skeleton, Spinner } from "@/components/ui";
import { ApiError, getDownloads } from "@/lib/api";

/** How long to keep waiting for the webhook before offering a manual retry. */
const CONFIRM_TIMEOUT_MS = 45_000;
const POLL_INTERVAL_MS = 2_000;

function formatSize(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/**
 * Shows a buyer their downloads after payment.
 *
 * The important part is what this does while waiting. Razorpay's browser
 * callback means "the payment window closed successfully" — it does not mean
 * our system knows about it. The order becomes paid when a signature-verified
 * webhook arrives at the API, which is usually a second or two later and
 * occasionally longer. So this polls the download endpoint, which answers 404
 * until the entitlement exists, and succeeds the moment it does.
 *
 * Polling rather than trusting the callback is the whole reason a forged
 * callback cannot unlock a file: the only thing that unlocks it is a row the
 * webhook wrote.
 */
export default function OrderDownloads({ orderId, initialEmail }) {
  const [email, setEmail] = useState(initialEmail);
  const [files, setFiles] = useState(null);
  const [phase, setPhase] = useState(initialEmail ? "confirming" : "ask-email");
  const [error, setError] = useState(null);

  // Set when polling starts, not during render: calling Date.now() while
  // rendering is impure, and React may render more than once.
  const startedAt = useRef(0);
  const cancelled = useRef(false);

  const poll = useCallback(
    async (address) => {
      cancelled.current = false;
      startedAt.current = Date.now();

      // No setState before the first await. Called from an effect, a
      // synchronous update here would cascade renders; the initial phase
      // already says "confirming", and the form path resets state itself.
      while (!cancelled.current) {
        try {
          const result = await getDownloads(orderId, address);
          if (cancelled.current) return;
          setFiles(result.files);
          setPhase("ready");
          return;
        } catch (err) {
          // 404 is the expected answer while the webhook is still in flight —
          // the entitlement does not exist yet. Anything else is a real error.
          const stillWaiting = err instanceof ApiError && err.isNotFound;
          if (!stillWaiting) {
            if (cancelled.current) return;
            setError(
              err instanceof ApiError && err.status === 429
                ? "Too many attempts. Wait a minute and try again."
                : "Something went wrong. Try again in a moment.",
            );
            setPhase("failed");
            return;
          }
        }

        if (Date.now() - startedAt.current > CONFIRM_TIMEOUT_MS) {
          // Not an error: a webhook can be delayed, and the purchase is safe
          // either way. Hand control back rather than spinning forever.
          setPhase("slow");
          return;
        }

        await new Promise((resolve) => setTimeout(resolve, POLL_INTERVAL_MS));
      }
    },
    [orderId],
  );

  useEffect(() => {
    if (!initialEmail) return undefined;

    // Started from a timer rather than called directly, so every state update
    // happens in a callback instead of while this effect body is running. That
    // is the pattern React's own guidance describes for subscribing to an
    // external system, and it is what the loop genuinely is: the API decides
    // when the entitlement exists, not this component.
    const timer = setTimeout(() => poll(initialEmail), 0);

    return () => {
      clearTimeout(timer);
      // Stops the loop if the buyer navigates away mid-wait.
      cancelled.current = true;
    };
  }, [initialEmail, poll]);

  if (phase === "ready" && files) {
    return (
      <div className="animate-rise">
        <SuccessMark />
        <h1 className="mt-5 text-center text-2xl font-semibold text-ink">Payment complete</h1>
        <p className="mt-2 text-center text-sm text-ink-soft">
          Your files are ready. Links expire in a few minutes — download them now.
        </p>

        <ul className="stagger mt-8 flex flex-col gap-2.5">
          {files.map((file) => (
            <li key={file.url}>
              <Card className="flex items-center gap-3 px-4 py-3.5">
                <div className="min-w-0 flex-1">
                  <p className="truncate text-sm font-medium text-ink">{file.filename}</p>
                  <p className="mt-0.5 text-xs text-ink-muted">
                    {file.product} · {formatSize(file.size_bytes)}
                  </p>
                </div>
                <Button
                  as="a"
                  href={file.url}
                  size="sm"
                  // The file comes straight from S3, not through our API, so the
                  // browser must not try to navigate to it as a page.
                  download={file.filename}
                  rel="noopener"
                  className="shrink-0"
                >
                  Download
                </Button>
              </Card>
            </li>
          ))}
        </ul>

        <p className="mt-6 text-center text-xs text-ink-muted">
          Keep this page. You can return and enter your email to get fresh links.
        </p>
      </div>
    );
  }

  if (phase === "confirming") {
    return (
      <div className="animate-rise text-center">
        <div className="mx-auto flex size-12 items-center justify-center rounded-full bg-brand-50 text-brand-600 dark:bg-brand-900/40 dark:text-brand-300">
          <Spinner className="size-5" />
        </div>
        <h1 className="mt-5 text-2xl font-semibold text-ink">Confirming your payment</h1>
        <p className="mx-auto mt-2 max-w-sm text-sm text-ink-soft">
          This usually takes a few seconds. Your payment is already safe — we are
          waiting for the confirmation to reach us.
        </p>

        {/* Skeletons at the size of the real rows, so nothing jumps when the
            files arrive. */}
        <div className="mt-8 flex flex-col gap-2.5">
          <Skeleton className="h-16 w-full rounded-card" />
          <Skeleton className="h-16 w-full rounded-card" />
        </div>
      </div>
    );
  }

  // "slow", "failed" and "ask-email" all end in the same form: the buyer tells
  // us the address they paid with, and we look again.
  return (
    <div className="animate-rise">
      <h1 className="text-center text-2xl font-semibold text-ink">
        {phase === "slow" ? "Still confirming" : "Find your downloads"}
      </h1>
      <p className="mx-auto mt-2 max-w-sm text-center text-sm text-ink-soft">
        {phase === "slow"
          ? "Your payment went through but the confirmation has not reached us yet. Try again in a moment — nothing is lost."
          : "Enter the email address you bought with and we will fetch your files."}
      </p>

      <Card className="mt-8 p-5">
        <form
          onSubmit={(event) => {
            event.preventDefault();
            const trimmed = email.trim();
            if (!trimmed) return;
            setPhase("confirming");
            setError(null);
            poll(trimmed);
          }}
          className="flex flex-col gap-4"
          noValidate
        >
          <Field label="Email" htmlFor="download-email" error={error}>
            <Input
              id="download-email"
              type="email"
              autoComplete="email"
              placeholder="you@example.com"
              value={email}
              invalid={Boolean(error)}
              onChange={(event) => setEmail(event.target.value)}
            />
          </Field>
          <Button type="submit" size="lg" className="w-full">
            {phase === "slow" ? "Check again" : "Get my files"}
          </Button>
        </form>
      </Card>
    </div>
  );
}

function SuccessMark() {
  return (
    <div className="mx-auto flex size-12 items-center justify-center rounded-full bg-success/12 text-success">
      <svg className="size-6" viewBox="0 0 24 24" fill="none" aria-hidden="true">
        <path
          d="m5 12.5 4.5 4.5L19 7.5"
          stroke="currentColor"
          strokeWidth="2.25"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      </svg>
    </div>
  );
}
