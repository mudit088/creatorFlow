"use client";

import Link from "next/link";
import { useCallback, useEffect, useState } from "react";

import { useAuth } from "@/components/AuthProvider";
import LinksEditor from "@/components/LinksEditor";
import { Button, Card, Field, Input, Skeleton } from "@/components/ui";
import { ApiError } from "@/lib/api";

export default function PageEditor() {
  const { authedRequest, status } = useAuth();

  const [profile, setProfile] = useState(null);
  const [links, setLinks] = useState([]);
  const [loadState, setLoadState] = useState("loading");

  const load = useCallback(async () => {
    try {
      const me = await authedRequest("/api/v1/profiles/me");
      setProfile(me);
      setLinks(me.links || []);
      setLoadState("ready");
    } catch (error) {
      setLoadState(error instanceof ApiError && error.isNotFound ? "no-profile" : "failed");
    }
  }, [authedRequest]);

  useEffect(() => {
    if (status !== "authenticated") return undefined;
    const timer = setTimeout(load, 0);
    return () => clearTimeout(timer);
  }, [status, load]);

  if (loadState === "loading") {
    return (
      <div>
        <Skeleton className="h-8 w-40" />
        <Skeleton className="mt-6 h-64 w-full rounded-card" />
      </div>
    );
  }

  if (loadState === "no-profile") {
    return (
      <Card className="p-6 text-center">
        <p className="text-sm text-ink-soft">Claim your page first.</p>
        <Button as={Link} href="/dashboard" size="sm" className="mt-4">
          Go to setup
        </Button>
      </Card>
    );
  }

  if (loadState === "failed") {
    return (
      <Card className="p-6 text-center">
        <p className="text-sm text-ink-soft">Could not load your page.</p>
        <Button variant="secondary" size="sm" className="mt-4" onClick={load}>
          Try again
        </Button>
      </Card>
    );
  }

  return (
    <div className="animate-rise flex flex-col gap-8">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold text-ink">Your page</h1>
          <p className="mt-1 text-sm text-ink-muted">
            creatorflow.app/@{profile.username}
          </p>
        </div>
        <Button as={Link} href={`/@${profile.username}`} variant="secondary" size="sm" target="_blank">
          Preview
        </Button>
      </div>

      <ProfileForm profile={profile} onSaved={setProfile} />
      <LinksEditor links={links} onChange={setLinks} />
    </div>
  );
}

/**
 * Profile details and the publish switch.
 *
 * Publishing is on this form rather than a separate screen, because "is anyone
 * able to see this?" is the single most important fact about a page and hiding
 * it one click away is how creators end up with a page nobody can visit.
 */
function ProfileForm({ profile, onSaved }) {
  const { authedRequest } = useAuth();

  const [displayName, setDisplayName] = useState(profile.display_name);
  const [bio, setBio] = useState(profile.bio || "");
  const [fieldErrors, setFieldErrors] = useState({});
  const [state, setState] = useState("idle"); // idle | saving | saved | failed

  const dirty = displayName !== profile.display_name || bio !== (profile.bio || "");

  async function save(patch, { silent = false } = {}) {
    if (!silent) setState("saving");
    setFieldErrors({});
    try {
      const updated = await authedRequest("/api/v1/profiles/me/update", {
        method: "POST",
        body: patch,
      });
      onSaved(updated);
      if (!silent) {
        setState("saved");
        // The confirmation fades on its own. A toast that must be dismissed is
        // another thing to click for something that already worked.
        setTimeout(() => setState("idle"), 2200);
      }
      return updated;
    } catch (error) {
      if (error instanceof ApiError && error.isValidation && error.fields) {
        setFieldErrors(error.fields);
        setState("idle");
        return null;
      }
      setState("failed");
      return null;
    }
  }

  return (
    <section>
      <Card className="p-5">
        <form
          onSubmit={(event) => {
            event.preventDefault();
            save({ display_name: displayName.trim(), bio: bio.trim() || null });
          }}
          className="flex flex-col gap-4"
          noValidate
        >
          <Field label="Display name" htmlFor="display-name" error={fieldErrors.display_name}>
            <Input
              id="display-name"
              value={displayName}
              invalid={Boolean(fieldErrors.display_name)}
              onChange={(e) => setDisplayName(e.target.value)}
            />
          </Field>

          <Field
            label="Bio"
            htmlFor="bio"
            error={fieldErrors.bio}
            hint={`${bio.length}/280 · one or two lines works best`}
          >
            <textarea
              id="bio"
              rows={3}
              maxLength={280}
              value={bio}
              onChange={(e) => setBio(e.target.value)}
              className="w-full resize-none rounded-control border border-line bg-card px-3.5 py-2.5 text-sm text-ink placeholder:text-ink-muted transition-colors hover:border-line-strong"
              placeholder="What you make, and who it is for."
            />
          </Field>

          <div className="flex items-center gap-3">
            <Button type="submit" loading={state === "saving"} disabled={!dirty}>
              Save changes
            </Button>
            {/* A live region, so the confirmation is announced rather than only
                being visible. */}
            <span aria-live="polite" className="text-sm text-ink-muted">
              {state === "saved" ? "Saved" : state === "failed" ? "Could not save" : ""}
            </span>
          </div>
        </form>
      </Card>

      <PublishRow profile={profile} onToggle={(next) => save({ is_published: next }, { silent: true })} />
    </section>
  );
}

function PublishRow({ profile, onToggle }) {
  const [busy, setBusy] = useState(false);
  const published = profile.is_published;

  return (
    <Card className="mt-3 flex flex-wrap items-center justify-between gap-3 p-4">
      <div className="flex items-center gap-3">
        <span
          aria-hidden="true"
          className={`size-2 rounded-full ${published ? "bg-success" : "bg-ink-muted"}`}
        />
        <div>
          <p className="text-sm font-medium text-ink">{published ? "Published" : "Draft"}</p>
          <p className="text-xs text-ink-muted">
            {published
              ? "Anyone with the link can see your page."
              : "Only you can see it. Products cannot be bought while it is a draft."}
          </p>
        </div>
      </div>
      <Button
        variant={published ? "secondary" : "primary"}
        size="sm"
        loading={busy}
        onClick={async () => {
          setBusy(true);
          await onToggle(!published);
          setBusy(false);
        }}
      >
        {published ? "Unpublish" : "Publish page"}
      </Button>
    </Card>
  );
}
