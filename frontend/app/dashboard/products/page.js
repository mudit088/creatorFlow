"use client";

import Link from "next/link";
import { useCallback, useEffect, useState } from "react";

import { useAuth } from "@/components/AuthProvider";
import ProductCard from "@/components/ProductCard";
import { Button, Card, Field, Input, Skeleton } from "@/components/ui";
import { ApiError } from "@/lib/api";

export default function ProductsPage() {
  const { authedRequest, status } = useAuth();

  const [products, setProducts] = useState([]);
  const [profile, setProfile] = useState(null);
  const [loadState, setLoadState] = useState("loading");
  const [creating, setCreating] = useState(false);

  const load = useCallback(async () => {
    try {
      const [me, list] = await Promise.all([
        authedRequest("/api/v1/profiles/me"),
        authedRequest("/api/v1/products"),
      ]);
      setProfile(me);
      setProducts(list.products || []);
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
        <Skeleton className="mt-6 h-32 w-full rounded-card" />
        <Skeleton className="mt-3 h-32 w-full rounded-card" />
      </div>
    );
  }

  if (loadState === "no-profile") {
    return (
      <Card className="p-6 text-center">
        <p className="text-sm text-ink-soft">Claim your page before adding products.</p>
        <Button as={Link} href="/dashboard" size="sm" className="mt-4">
          Go to setup
        </Button>
      </Card>
    );
  }

  return (
    <div className="animate-rise">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold text-ink">Products</h1>
        <Button size="sm" onClick={() => setCreating((open) => !open)}>
          {creating ? "Cancel" : "New product"}
        </Button>
      </div>

      {!profile?.is_published ? (
        <Card className="mt-4 border-brand-200 bg-brand-50/60 p-4 dark:border-brand-800 dark:bg-brand-900/20">
          <p className="text-sm text-ink-soft">
            Your page is a draft, so nothing can be bought yet.{" "}
            <Link href="/dashboard/page-editor" className="font-medium text-brand-700 underline-offset-4 hover:underline dark:text-brand-300">
              Publish your page
            </Link>{" "}
            when you are ready.
          </p>
        </Card>
      ) : null}

      {creating ? (
        <NewProductForm
          onCreated={(product) => {
            setProducts((current) => [product, ...current]);
            setCreating(false);
          }}
        />
      ) : null}

      {products.length === 0 && !creating ? (
        <Card className="mt-6 p-8 text-center">
          <p className="text-sm font-medium text-ink">Nothing for sale yet</p>
          <p className="mx-auto mt-1 max-w-sm text-sm text-ink-muted">
            A product is a file plus a price. Upload the file, set a price, and
            publish it to your page.
          </p>
          <Button size="sm" className="mt-5" onClick={() => setCreating(true)}>
            Create your first product
          </Button>
        </Card>
      ) : (
        <ul className="mt-6 flex flex-col gap-3">
          {products.map((product) => (
            <li key={product.id}>
              <ProductCard
                product={product}
                username={profile?.username}
                onChange={(updated) =>
                  setProducts((current) =>
                    current.map((item) => (item.id === updated.id ? updated : item)),
                  )
                }
              />
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/**
 * Creating a product takes three fields and nothing else.
 *
 * The file comes after, because uploading is the slow part and asking for it up
 * front means a failed upload loses the title and price too. The API agrees: a
 * product starts as a draft and cannot be published until a file is confirmed.
 */
function NewProductForm({ onCreated }) {
  const { authedRequest } = useAuth();
  const [title, setTitle] = useState("");
  const [price, setPrice] = useState("");
  const [description, setDescription] = useState("");
  const [fieldErrors, setFieldErrors] = useState({});
  const [busy, setBusy] = useState(false);

  /** "yourname" from "Your Name!" — a readable slug the API will accept. */
  function slugify(value) {
    return value
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-+|-+$/g, "")
      .slice(0, 60);
  }

  async function handleSubmit(event) {
    event.preventDefault();
    setFieldErrors({});
    setBusy(true);

    // Rupees in the form, paise over the wire. The conversion happens once,
    // here, and Math.round keeps 49.99 from arriving as 4998.999999999999.
    const rupees = Number(price);
    if (!Number.isFinite(rupees) || rupees <= 0) {
      setFieldErrors({ price_minor: "Enter a price greater than zero." });
      setBusy(false);
      return;
    }

    try {
      const product = await authedRequest("/api/v1/products", {
        method: "POST",
        body: {
          slug: slugify(title),
          title: title.trim(),
          description: description.trim() || null,
          price_minor: Math.round(rupees * 100),
        },
      });
      onCreated(product);
    } catch (error) {
      if (error instanceof ApiError && error.isValidation && error.fields) {
        setFieldErrors(error.fields);
      } else if (error instanceof ApiError && error.status === 409) {
        setFieldErrors({ title: "You already have a product with a similar name." });
      } else {
        setFieldErrors({ title: "Could not create that product." });
      }
      setBusy(false);
    }
  }

  return (
    <Card className="mt-6 p-5 shadow-raised">
      <form onSubmit={handleSubmit} className="flex flex-col gap-4" noValidate>
        <Field label="Title" htmlFor="product-title" error={fieldErrors.title || fieldErrors.slug}>
          <Input
            id="product-title"
            autoFocus
            placeholder="The Design Systems Handbook"
            value={title}
            invalid={Boolean(fieldErrors.title || fieldErrors.slug)}
            onChange={(e) => setTitle(e.target.value)}
            disabled={busy}
          />
        </Field>

        <Field
          label="Price"
          htmlFor="product-price"
          error={fieldErrors.price_minor}
          hint="In rupees. Buyers pay this once."
        >
          <div className="flex items-center gap-2">
            <span className="text-sm text-ink-muted">₹</span>
            <Input
              id="product-price"
              inputMode="decimal"
              placeholder="499"
              value={price}
              invalid={Boolean(fieldErrors.price_minor)}
              onChange={(e) => setPrice(e.target.value)}
              disabled={busy}
            />
          </div>
        </Field>

        <Field label="Description" htmlFor="product-description" error={fieldErrors.description}>
          <textarea
            id="product-description"
            rows={3}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            disabled={busy}
            placeholder="What it is, and who it helps."
            className="w-full resize-none rounded-control border border-line bg-card px-3.5 py-2.5 text-sm text-ink placeholder:text-ink-muted"
          />
        </Field>

        <Button type="submit" loading={busy} className="self-start">
          Create product
        </Button>
      </form>
    </Card>
  );
}
