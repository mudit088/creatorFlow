"use client";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { useState } from "react";

import { useAuth } from "@/components/AuthProvider";
import { Button, Card, Field, Input } from "@/components/ui";
import { ApiError } from "@/lib/api";

/**
 * Sign in and sign up are the same form with different copy, so they are one
 * component. Two near-identical files is how the two screens drift until one
 * of them is missing a validation message nobody noticed.
 */
export default function AuthForm({ mode }) {
  const isRegister = mode === "register";
  const router = useRouter();
  const params = useSearchParams();
  const { login, register } = useAuth();

  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [fieldErrors, setFieldErrors] = useState({});
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);

  async function handleSubmit(event) {
    event.preventDefault();
    setError(null);
    setFieldErrors({});
    setBusy(true);

    try {
      await (isRegister ? register(email.trim(), password) : login(email.trim(), password));
      // `next` lets a protected page send someone here and get them back where
      // they were going, rather than dumping everyone on the dashboard root.
      router.replace(params.get("next") || "/dashboard");
    } catch (err) {
      setBusy(false);
      if (err instanceof ApiError && err.isValidation && err.fields) {
        setFieldErrors(err.fields);
        return;
      }
      if (err instanceof ApiError && err.status === 401) {
        // The API deliberately answers the same for "no such account" and
        // "wrong password", so this copy must not imply which it was.
        setError("That email and password do not match an account.");
        return;
      }
      if (err instanceof ApiError && err.status === 409) {
        setError("An account with that email already exists. Sign in instead.");
        return;
      }
      if (err instanceof ApiError && err.status === 429) {
        setError("Too many attempts. Wait a minute and try again.");
        return;
      }
      setError("Something went wrong. Please try again.");
    }
  }

  return (
    <main className="mx-auto flex min-h-screen w-full max-w-sm flex-col justify-center px-5 py-12">
      <div className="animate-rise">
        <Link href="/" className="text-sm font-semibold tracking-tight text-ink">
          CreatorFlow
        </Link>
        <h1 className="mt-6 text-2xl font-semibold text-ink">
          {isRegister ? "Create your account" : "Welcome back"}
        </h1>
        <p className="mt-1.5 text-sm text-ink-soft">
          {isRegister
            ? "Your page and products take a couple of minutes to set up."
            : "Sign in to manage your page, products and sales."}
        </p>

        <Card className="mt-7 p-5 shadow-raised">
          <form onSubmit={handleSubmit} className="flex flex-col gap-4" noValidate>
            <Field label="Email" htmlFor="email" error={fieldErrors.email}>
              <Input
                id="email"
                type="email"
                autoComplete="email"
                autoFocus
                placeholder="you@example.com"
                value={email}
                invalid={Boolean(fieldErrors.email)}
                onChange={(e) => setEmail(e.target.value)}
                disabled={busy}
              />
            </Field>

            <Field
              label="Password"
              htmlFor="password"
              error={fieldErrors.password}
              hint={isRegister ? "At least 12 characters." : undefined}
            >
              <Input
                id="password"
                type="password"
                // new-password tells a password manager to offer a generated
                // one on signup, and not to overwrite a saved one on sign-in.
                autoComplete={isRegister ? "new-password" : "current-password"}
                placeholder="••••••••••••"
                value={password}
                invalid={Boolean(fieldErrors.password)}
                onChange={(e) => setPassword(e.target.value)}
                disabled={busy}
              />
            </Field>

            <Button type="submit" size="lg" loading={busy} className="w-full">
              {isRegister ? "Create account" : "Sign in"}
            </Button>

            {error ? (
              <p role="alert" className="text-sm text-danger">
                {error}
              </p>
            ) : null}
          </form>
        </Card>

        <p className="mt-5 text-center text-sm text-ink-soft">
          {isRegister ? "Already have an account? " : "New here? "}
          <Link
            href={isRegister ? "/login" : "/register"}
            className="font-medium text-brand-600 underline-offset-4 hover:underline dark:text-brand-300"
          >
            {isRegister ? "Sign in" : "Create one"}
          </Link>
        </p>
      </div>
    </main>
  );
}
