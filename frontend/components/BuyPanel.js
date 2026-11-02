"use client";

import { useRouter } from "next/navigation";
import { useRef, useState } from "react";

import { Button, Card, Field, Input } from "@/components/ui";
import { ApiError, createOrder } from "@/lib/api";

const RAZORPAY_SCRIPT = "https://checkout.razorpay.com/v1/checkout.js";

/**
 * Loads Razorpay's checkout script once, on demand.
 *
 * Deliberately not in the page's <head>: it is a third-party script on every
 * product page, and most visitors never buy. Loading it when someone actually
 * submits the form keeps it off the critical path for everyone else, and the
 * promise is cached so a second attempt does not fetch it again.
 */
let scriptPromise = null;
function loadRazorpay() {
  if (window.Razorpay) return Promise.resolve(true);
  if (scriptPromise) return scriptPromise;

  scriptPromise = new Promise((resolve) => {
    const script = document.createElement("script");
    script.src = RAZORPAY_SCRIPT;
    script.onload = () => resolve(true);
    script.onerror = () => {
      // Let a later attempt retry rather than caching the failure forever —
      // this fails for ad blockers and flaky networks, both of which recover.
      scriptPromise = null;
      resolve(false);
    };
    document.body.appendChild(script);
  });
  return scriptPromise;
}

export default function BuyPanel({ productId, priceLabel, productTitle, creatorName }) {
  const router = useRouter();
  const [email, setEmail] = useState("");
  const [status, setStatus] = useState("idle"); // idle | working | paying
  const [error, setError] = useState(null);
  const [fieldError, setFieldError] = useState(null);

  /**
   * One key per attempt, held across retries.
   *
   * This is what makes a double-click or a retry after a network blip reuse the
   * same order instead of creating a second one — the API returns the original
   * for a repeated key. Regenerating it on every submit would defeat the
   * mechanism entirely.
   */
  const idempotencyKey = useRef(null);

  async function handleSubmit(event) {
    event.preventDefault();
    setError(null);
    setFieldError(null);

    const trimmed = email.trim();
    if (!trimmed) {
      setFieldError("Enter the email your files should be sent to.");
      return;
    }

    setStatus("working");
    if (!idempotencyKey.current) {
      idempotencyKey.current = crypto.randomUUID();
    }

    let order;
    try {
      order = await createOrder({
        buyerEmail: trimmed,
        productIds: [productId],
        idempotencyKey: idempotencyKey.current,
      });
    } catch (err) {
      setStatus("idle");
      if (err instanceof ApiError && err.isValidation) {
        // The API returns which field it rejected, so the message lands on the
        // input rather than in a banner the user has to map back themselves.
        setFieldError(err.fields?.buyer_email || err.fields?.product_ids || err.message);
        return;
      }
      setError(
        err instanceof ApiError && err.code === "payment_provider_unavailable"
          ? "Payments are temporarily unavailable. Try again in a moment."
          : "Something went wrong creating your order. Please try again.",
      );
      return;
    }

    // No checkout block means the server has no payment keys configured. Saying
    // so plainly beats opening an empty payment window.
    if (!order.checkout) {
      setStatus("idle");
      setError("Payments are not configured for this store yet.");
      return;
    }

    const ready = await loadRazorpay();
    if (!ready) {
      setStatus("idle");
      setError("Could not load the payment window. Check your connection or any ad blocker.");
      return;
    }

    setStatus("paying");

    const checkout = new window.Razorpay({
      key: order.checkout.key_id,
      order_id: order.checkout.provider_order_id,
      amount: order.checkout.amount_minor,
      currency: order.checkout.currency,
      name: creatorName,
      description: productTitle,
      prefill: { email: trimmed },
      theme: { color: "#7c3aed" },
      handler: () => {
        // Payment taken. Note what does NOT happen here: the frontend does not
        // tell the API the order is paid. Only a signature-verified webhook can
        // do that, because anything this code sends can be forged by whoever is
        // holding the browser. The next page waits for that webhook to land.
        router.push(`/orders/${order.id}?email=${encodeURIComponent(trimmed)}`);
      },
      modal: {
        ondismiss: () => {
          // The order stays pending and a background job expires it after a
          // day — so a closed window costs nothing and the same key resumes it.
          setStatus("idle");
        },
      },
    });

    checkout.on("payment.failed", (response) => {
      setStatus("idle");
      setError(response?.error?.description || "The payment did not go through. You can try again.");
    });

    checkout.open();
  }

  const busy = status !== "idle";

  return (
    <Card className="animate-rise mt-10 p-5 shadow-raised">
      <div className="flex items-baseline justify-between gap-4">
        <span className="text-sm font-medium text-ink-soft">One-time payment</span>
        <span className="text-2xl font-semibold tabular-nums text-ink">{priceLabel}</span>
      </div>

      <form onSubmit={handleSubmit} className="mt-5 flex flex-col gap-4" noValidate>
        <Field
          label="Email"
          htmlFor="buyer-email"
          error={fieldError}
          hint="Your download link is tied to this address."
        >
          <Input
            id="buyer-email"
            name="email"
            type="email"
            autoComplete="email"
            inputMode="email"
            placeholder="you@example.com"
            value={email}
            invalid={Boolean(fieldError)}
            onChange={(event) => setEmail(event.target.value)}
            disabled={busy}
          />
        </Field>

        <Button type="submit" size="lg" loading={busy} className="w-full">
          {status === "paying" ? "Opening payment…" : `Buy for ${priceLabel}`}
        </Button>

        {error ? (
          // A live region: an error that appears after submission is announced,
          // rather than only being found by someone who navigates back up.
          <p role="alert" className="text-sm text-danger">
            {error}
          </p>
        ) : null}

        <p className="text-center text-xs text-ink-muted">
          Secure payment via Razorpay. Files are delivered immediately after payment.
        </p>
      </form>
    </Card>
  );
}
