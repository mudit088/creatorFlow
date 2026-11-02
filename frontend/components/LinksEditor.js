"use client";

import { useState } from "react";

import { useAuth } from "@/components/AuthProvider";
import { Button, Card, Field, Input } from "@/components/ui";
import { ApiError } from "@/lib/api";

/**
 * Add, edit, reorder and remove the links on a creator's page.
 *
 * Reordering is buttons rather than drag-and-drop. Drag is nicer with a mouse
 * and unusable without one — no keyboard, poor screen-reader story, and fiddly
 * on a phone, which is where creators actually edit these. Buttons work
 * everywhere, and the API takes the whole ordered list either way.
 */
export default function LinksEditor({ links, onChange }) {
  const { authedRequest } = useAuth();
  const [error, setError] = useState(null);
  const [busyId, setBusyId] = useState(null);

  async function reorder(from, to) {
    if (to < 0 || to >= links.length) return;

    const next = [...links];
    const [moved] = next.splice(from, 1);
    next.splice(to, 0, moved);

    // Optimistic: the list reorders immediately and is rolled back if the API
    // refuses. Waiting for a round trip to move a row makes the whole page feel
    // slow, and this is the one action a creator does repeatedly.
    const previous = links;
    onChange(next);
    setError(null);

    try {
      await authedRequest("/api/v1/profiles/me/links/order", {
        method: "POST",
        body: { link_ids: next.map((link) => link.id) },
      });
    } catch {
      onChange(previous);
      setError("Could not save the new order.");
    }
  }

  async function remove(id) {
    const previous = links;
    onChange(links.filter((link) => link.id !== id));
    setBusyId(id);
    try {
      await authedRequest(`/api/v1/profiles/me/links/${id}/delete`, { method: "POST" });
    } catch {
      onChange(previous);
      setError("Could not remove that link.");
    } finally {
      setBusyId(null);
    }
  }

  async function toggleActive(link) {
    setBusyId(link.id);
    try {
      const updated = await authedRequest(`/api/v1/profiles/me/links/${link.id}/update`, {
        method: "POST",
        body: { is_active: !link.is_active },
      });
      onChange(links.map((item) => (item.id === link.id ? updated : item)));
    } catch {
      setError("Could not update that link.");
    } finally {
      setBusyId(null);
    }
  }

  return (
    <section aria-labelledby="links-heading">
      <div className="mb-3 flex items-baseline justify-between">
        <h2 id="links-heading" className="text-sm font-semibold text-ink">
          Links
        </h2>
        <span className="text-xs text-ink-muted">{links.length} on your page</span>
      </div>

      {links.length > 0 ? (
        <ul className="flex flex-col gap-2">
          {links.map((link, index) => (
            <li key={link.id}>
              <Card className="flex items-center gap-3 px-4 py-3">
                <div className="flex shrink-0 flex-col">
                  <IconButton
                    label={`Move ${link.title} up`}
                    disabled={index === 0}
                    onClick={() => reorder(index, index - 1)}
                  >
                    <ChevronIcon className="size-3.5 rotate-180" />
                  </IconButton>
                  <IconButton
                    label={`Move ${link.title} down`}
                    disabled={index === links.length - 1}
                    onClick={() => reorder(index, index + 1)}
                  >
                    <ChevronIcon className="size-3.5" />
                  </IconButton>
                </div>

                <div className="min-w-0 flex-1">
                  <p
                    className={`truncate text-sm font-medium ${
                      link.is_active ? "text-ink" : "text-ink-muted line-through"
                    }`}
                  >
                    {link.title}
                  </p>
                  <p className="truncate text-xs text-ink-muted">{link.url}</p>
                </div>

                <Button
                  variant="ghost"
                  size="sm"
                  loading={busyId === link.id}
                  onClick={() => toggleActive(link)}
                >
                  {link.is_active ? "Hide" : "Show"}
                </Button>
                <Button variant="ghost" size="sm" onClick={() => remove(link.id)}>
                  Remove
                </Button>
              </Card>
            </li>
          ))}
        </ul>
      ) : (
        <Card className="px-4 py-6 text-center">
          <p className="text-sm text-ink-muted">No links yet.</p>
        </Card>
      )}

      {error ? (
        <p role="alert" className="mt-2 text-sm text-danger">
          {error}
        </p>
      ) : null}

      <AddLinkForm
        onAdded={(link) => {
          onChange([...links, link]);
          setError(null);
        }}
      />
    </section>
  );
}

function AddLinkForm({ onAdded }) {
  const { authedRequest } = useAuth();
  const [title, setTitle] = useState("");
  const [url, setUrl] = useState("");
  const [fieldErrors, setFieldErrors] = useState({});
  const [busy, setBusy] = useState(false);

  async function handleSubmit(event) {
    event.preventDefault();
    setFieldErrors({});
    setBusy(true);

    // A creator typing "example.com" means https. Rejecting it would be
    // technically correct and unhelpful; the API requires a scheme, so add the
    // one they meant rather than making them.
    const normalised = /^https?:\/\//i.test(url.trim()) ? url.trim() : `https://${url.trim()}`;

    try {
      const link = await authedRequest("/api/v1/profiles/me/links", {
        method: "POST",
        body: { title: title.trim(), url: normalised },
      });
      onAdded(link);
      setTitle("");
      setUrl("");
    } catch (error) {
      if (error instanceof ApiError && error.isValidation && error.fields) {
        setFieldErrors(error.fields);
      } else if (error instanceof ApiError && error.status === 409) {
        setFieldErrors({ title: "You have reached the maximum number of links." });
      } else {
        setFieldErrors({ title: "Could not add that link." });
      }
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card className="mt-3 p-4">
      <form onSubmit={handleSubmit} className="flex flex-col gap-3 sm:flex-row sm:items-start" noValidate>
        <div className="flex-1">
          <Field label="Title" htmlFor="link-title" error={fieldErrors.title}>
            <Input
              id="link-title"
              placeholder="YouTube"
              value={title}
              invalid={Boolean(fieldErrors.title)}
              onChange={(e) => setTitle(e.target.value)}
              disabled={busy}
            />
          </Field>
        </div>
        <div className="flex-1">
          <Field label="URL" htmlFor="link-url" error={fieldErrors.url}>
            <Input
              id="link-url"
              inputMode="url"
              placeholder="youtube.com/@you"
              value={url}
              invalid={Boolean(fieldErrors.url)}
              onChange={(e) => setUrl(e.target.value)}
              disabled={busy}
            />
          </Field>
        </div>
        <Button type="submit" loading={busy} className="sm:mt-[1.6rem]">
          Add link
        </Button>
      </form>
    </Card>
  );
}

function IconButton({ label, disabled, onClick, children }) {
  return (
    <button
      type="button"
      // The label is the accessible name: the icon alone says nothing to a
      // screen reader, and "button" is not a useful announcement.
      aria-label={label}
      disabled={disabled}
      onClick={onClick}
      className="rounded px-1 py-0.5 text-ink-muted transition-colors hover:text-ink disabled:opacity-30 disabled:hover:text-ink-muted"
    >
      {children}
    </button>
  );
}

function ChevronIcon({ className }) {
  return (
    <svg className={className} viewBox="0 0 16 16" fill="none" aria-hidden="true">
      <path d="M3.5 6 8 10.5 12.5 6" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}
