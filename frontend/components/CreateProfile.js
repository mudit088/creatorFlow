"use client";

import { useState } from "react";

import { useAuth } from "@/components/AuthProvider";
import { Button, Card, Field, Input } from "@/components/ui";
import { ApiError } from "@/lib/api";

/**
 * First run: a creator has an account but no page yet.
 *
 * Deliberately one screen with two fields. Everything else — bio, links,
 * products, publishing — can be edited afterwards, and asking for it now is how
 * a setup flow loses the person before they have anything to show.
 */
export default function CreateProfile({ onCreated }) {
  const { authedRequest } = useAuth();
  const [username, setUsername] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [fieldErrors, setFieldErrors] = useState({});
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);

  async function handleSubmit(event) {
    event.preventDefault();
    setError(null);
    setFieldErrors({});
    setBusy(true);

    try {
      await authedRequest("/api/v1/profiles", {
        method: "POST",
        body: { username: username.trim().toLowerCase(), display_name: displayName.trim() },
      });
      onCreated();
    } catch (err) {
      setBusy(false);
      if (err instanceof ApiError && err.isValidation && err.fields) {
        setFieldErrors(err.fields);
        return;
      }
      if (err instanceof ApiError && err.status === 409) {
        // The API distinguishes "taken" from "reserved"; both mean the same
        // thing to someone choosing a handle.
        setFieldErrors({ username: "That handle is taken. Try another." });
        return;
      }
      setError("Could not create your page. Please try again.");
    }
  }

  return (
    <div className="animate-rise mx-auto max-w-md">
      <h1 className="text-2xl font-semibold text-ink">Claim your page</h1>
      <p className="mt-1.5 text-sm text-ink-soft">
        Pick the handle your page lives at. You can change everything else later.
      </p>

      <Card className="mt-6 p-5 shadow-raised">
        <form onSubmit={handleSubmit} className="flex flex-col gap-4" noValidate>
          <Field
            label="Handle"
            htmlFor="username"
            error={fieldErrors.username}
            hint="Lowercase letters, numbers and underscores."
          >
            {/* The prefix is shown, not typed: it makes the resulting URL
                obvious and stops people typing the "@" and the domain
                themselves, which is the most common way this field gets
                filled wrong. */}
            <div className="flex items-center gap-0 overflow-hidden rounded-control border border-line bg-card focus-within:border-brand-400">
              <span className="pl-3.5 pr-0.5 text-sm text-ink-muted">creatorflow.app/@</span>
              <Input
                id="username"
                autoFocus
                autoComplete="off"
                autoCapitalize="none"
                spellCheck={false}
                placeholder="yourname"
                value={username}
                invalid={Boolean(fieldErrors.username)}
                onChange={(e) => setUsername(e.target.value)}
                disabled={busy}
                className="border-0 bg-transparent pl-0 focus-visible:outline-none"
              />
            </div>
          </Field>

          <Field label="Display name" htmlFor="display-name" error={fieldErrors.display_name}>
            <Input
              id="display-name"
              placeholder="Your name"
              value={displayName}
              invalid={Boolean(fieldErrors.display_name)}
              onChange={(e) => setDisplayName(e.target.value)}
              disabled={busy}
            />
          </Field>

          <Button type="submit" size="lg" loading={busy} className="w-full">
            Create my page
          </Button>

          {error ? (
            <p role="alert" className="text-sm text-danger">
              {error}
            </p>
          ) : null}
        </form>
      </Card>
    </div>
  );
}
