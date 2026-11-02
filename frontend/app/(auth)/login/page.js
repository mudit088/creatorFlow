import { Suspense } from "react";

import AuthForm from "@/components/AuthForm";

export const metadata = { title: "Sign in" };

export default function LoginPage() {
  // useSearchParams needs a Suspense boundary: without one the whole route
  // opts out of static rendering and Next fails the build.
  return (
    <Suspense>
      <AuthForm mode="login" />
    </Suspense>
  );
}
