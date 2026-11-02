"use client";

import Link from "next/link";
import { useRef, useState } from "react";

import { useAuth } from "@/components/AuthProvider";
import { Badge, Button, Card, Spinner } from "@/components/ui";
import { ApiError, formatMoney } from "@/lib/api";

function formatSize(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

export default function ProductCard({ product, username, onChange }) {
  const { authedRequest, token } = useAuth();

  const [files, setFiles] = useState(null); // null until expanded
  const [expanded, setExpanded] = useState(false);
  const [upload, setUpload] = useState(null); // {name, percent} while in flight
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const inputRef = useRef(null);

  const published = product.status === "published";

  async function toggleFiles() {
    const next = !expanded;
    setExpanded(next);
    if (next && files === null) {
      try {
        const detail = await authedRequest(`/api/v1/products/${product.id}`);
        setFiles(detail.files || []);
      } catch {
        setError("Could not load files.");
      }
    }
  }

  /**
   * Uploads straight from the browser to S3.
   *
   * Three steps, and the middle one never touches our API: ask for a presigned
   * URL, PUT the bytes to storage, then tell the API to verify what landed. A
   * 2 GB file therefore costs the API two small JSON requests instead of 2 GB
   * of traffic and memory — which is why its body limit can stay at 2 MB.
   */
  async function handleFile(file) {
    if (!file) return;
    setError(null);
    setUpload({ name: file.name, percent: 0 });

    try {
      const intent = await authedRequest(`/api/v1/products/${product.id}/upload-url`, {
        method: "POST",
        body: {
          filename: file.name,
          content_type: file.type || "application/octet-stream",
          size_bytes: file.size,
        },
      });

      // XMLHttpRequest rather than fetch: fetch still cannot report upload
      // progress, and a large file with no progress bar looks like a hang.
      await new Promise((resolve, reject) => {
        const xhr = new XMLHttpRequest();
        xhr.open(intent.method || "PUT", intent.upload_url);

        // Exactly the headers the URL was signed for. The signature covers
        // Content-Type and Content-Length, so sending anything else — or
        // letting the browser add an Authorization header — produces a
        // SignatureDoesNotMatch from S3.
        Object.entries(intent.required_headers || {}).forEach(([key, value]) => {
          xhr.setRequestHeader(key, value);
        });

        xhr.upload.onprogress = (event) => {
          if (event.lengthComputable) {
            setUpload({ name: file.name, percent: Math.round((event.loaded / event.total) * 100) });
          }
        };
        xhr.onload = () =>
          xhr.status >= 200 && xhr.status < 300
            ? resolve()
            : reject(new Error(`storage responded ${xhr.status}`));
        xhr.onerror = () => reject(new Error("network error during upload"));
        xhr.send(file);
      });

      // The API asks S3 whether the object really landed and matches what was
      // declared. Until this succeeds the file row exists with no bytes behind
      // it, and the product cannot be published.
      const confirmed = await authedRequest(
        `/api/v1/products/${product.id}/files/${intent.file.id}/confirm`,
        { method: "POST" },
      );

      setFiles((current) => [...(current || []), confirmed]);
      setExpanded(true);
    } catch (err) {
      setError(
        err instanceof ApiError
          ? err.message
          : "The upload did not complete. Check the file and try again.",
      );
    } finally {
      setUpload(null);
      if (inputRef.current) inputRef.current.value = "";
    }
  }

  async function togglePublished() {
    setBusy(true);
    setError(null);
    try {
      const updated = await authedRequest(`/api/v1/products/${product.id}/update`, {
        method: "POST",
        body: { status: published ? "draft" : "published" },
      });
      onChange(updated);
    } catch (err) {
      // The API refuses to publish a product with nothing to deliver. That is
      // the single most useful error on this screen, so it is shown verbatim
      // rather than flattened into "something went wrong".
      setError(
        err instanceof ApiError && err.code === "no_deliverable"
          ? "Upload a file before publishing — buyers need something to download."
          : "Could not change the status.",
      );
    } finally {
      setBusy(false);
    }
  }

  async function removeFile(fileId) {
    setError(null);
    try {
      await authedRequest(`/api/v1/products/${product.id}/files/${fileId}/delete`, {
        method: "POST",
      });
      setFiles((current) => current.filter((f) => f.id !== fileId));
      // Removing the last file un-publishes the product server-side, so the
      // card would otherwise keep claiming it is live.
      if (files?.length === 1 && published) {
        onChange({ ...product, status: "draft", published_at: null });
      }
    } catch (err) {
      setError(
        err instanceof ApiError && err.code === "file_sold"
          ? "Someone has already bought this file, so it cannot be deleted. Archive the product instead."
          : "Could not remove that file.",
      );
    }
  }

  return (
    <Card className="p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <h2 className="truncate text-sm font-medium text-ink">{product.title}</h2>
            <StatusBadge status={product.status} />
          </div>
          <p className="mt-1 text-xs text-ink-muted">
            {formatMoney(product.price_minor, product.currency)} ·{" "}
            {published && username ? (
              <Link
                href={`/@${username}/${product.slug}`}
                target="_blank"
                className="underline-offset-4 hover:underline"
              >
                /{product.slug}
              </Link>
            ) : (
              <span>/{product.slug}</span>
            )}
          </p>
        </div>

        <div className="flex shrink-0 items-center gap-2">
          <Button variant="ghost" size="sm" onClick={toggleFiles}>
            {expanded ? "Hide files" : "Files"}
          </Button>
          <Button
            variant={published ? "secondary" : "primary"}
            size="sm"
            loading={busy}
            // Disabled while a file is still uploading. Publishing then asks
            // the API to make live a product whose only file is not confirmed
            // yet, and it rightly refuses — but the creator sees a 409 they did
            // nothing to deserve, seconds before it would have worked.
            disabled={Boolean(upload)}
            title={upload ? "Waiting for the upload to finish" : undefined}
            onClick={togglePublished}
          >
            {published ? "Unpublish" : "Publish"}
          </Button>
        </div>
      </div>

      {expanded ? (
        <div className="mt-4 border-t border-line pt-4">
          {files === null ? (
            <div className="flex items-center gap-2 text-sm text-ink-muted">
              <Spinner className="size-3.5" /> Loading files…
            </div>
          ) : (
            <>
              {files.length > 0 ? (
                <ul className="flex flex-col gap-2">
                  {files.map((file) => (
                    <li key={file.id} className="flex items-center gap-3 rounded-control bg-canvas px-3 py-2">
                      <span className="min-w-0 flex-1 truncate text-sm text-ink">
                        {file.original_name}
                      </span>
                      <span className="shrink-0 text-xs tabular-nums text-ink-muted">
                        {formatSize(file.size_bytes)}
                      </span>
                      {!file.uploaded ? (
                        <Badge className="bg-canvas text-ink-muted">incomplete</Badge>
                      ) : null}
                      <Button variant="ghost" size="sm" onClick={() => removeFile(file.id)}>
                        Remove
                      </Button>
                    </li>
                  ))}
                </ul>
              ) : (
                <p className="text-sm text-ink-muted">No files yet.</p>
              )}

              {upload ? (
                <div className="mt-3">
                  <div className="flex items-center justify-between text-xs text-ink-muted">
                    <span className="truncate">Uploading {upload.name}…</span>
                    <span className="tabular-nums">{upload.percent}%</span>
                  </div>
                  {/* A real progress bar, fed by upload events — not an
                      indeterminate spinner that tells the creator nothing
                      about whether a large file is moving. */}
                  <div className="mt-1.5 h-1.5 w-full overflow-hidden rounded-full bg-line">
                    <div
                      className="h-full rounded-full bg-brand-500 transition-[width] duration-200 ease-soft"
                      style={{ width: `${upload.percent}%` }}
                    />
                  </div>
                </div>
              ) : (
                <div className="mt-3">
                  <input
                    ref={inputRef}
                    id={`file-${product.id}`}
                    type="file"
                    className="sr-only"
                    onChange={(event) => handleFile(event.target.files?.[0])}
                  />
                  <Button as="label" htmlFor={`file-${product.id}`} variant="secondary" size="sm" className="cursor-pointer">
                    Upload a file
                  </Button>
                </div>
              )}
            </>
          )}
        </div>
      ) : null}

      {error ? (
        <p role="alert" className="mt-3 text-sm text-danger">
          {error}
        </p>
      ) : null}
    </Card>
  );
}

function StatusBadge({ status }) {
  if (status === "published") {
    return <Badge className="bg-success/12 text-success">Live</Badge>;
  }
  if (status === "archived") {
    return <Badge className="bg-canvas text-ink-muted">Archived</Badge>;
  }
  return <Badge className="bg-canvas text-ink-muted">Draft</Badge>;
}
