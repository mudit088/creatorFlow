/**
 * The only place this app talks to the Go API.
 *
 * Everything goes through here so that three decisions are made once: which
 * base URL to use, what an error looks like, and what a caller has to handle.
 * A component that calls fetch directly would have to re-make all three, and
 * would eventually make one of them differently.
 */

/**
 * Server components and the browser reach the API by different routes.
 *
 * Running natively both are http://localhost:8080 and this is invisible. Inside
 * Docker Compose the server side must use http://api:8080, because a server
 * component runs in the container where "localhost" is the container itself,
 * while the browser must use localhost:8080, because "api" means nothing on the
 * user's machine. Exactly the split the backend has for S3.
 */
function baseUrl() {
  // typeof window is the standard test for "am I on the server". On the server
  // API_INTERNAL_URL is available; in the browser only NEXT_PUBLIC_* variables
  // are, which is why the public one is named that way.
  if (typeof window === "undefined") {
    return process.env.API_INTERNAL_URL || process.env.NEXT_PUBLIC_API_URL || "http://localhost:8080";
  }
  return process.env.NEXT_PUBLIC_API_URL || "http://localhost:8080";
}

/**
 * ApiError carries the shape the Go API actually returns, so a caller can
 * switch on `code` rather than parsing a message that exists for humans.
 *
 * The backend returns one error shape everywhere — {code, message, fields?} —
 * and this is the client-side half of that contract.
 */
export class ApiError extends Error {
  constructor(status, code, message, fields) {
    super(message || "Something went wrong.");
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.fields = fields || null;
  }

  /** True when the API rejected the input and said which fields were wrong. */
  get isValidation() {
    return this.code === "validation_failed" || this.status === 422;
  }

  get isUnauthorized() {
    return this.status === 401;
  }

  get isNotFound() {
    return this.status === 404;
  }
}

/**
 * request performs one API call and normalises everything that can go wrong.
 *
 * Three failure modes are flattened into ApiError so callers handle one type:
 * the API returned a structured error, the API returned something unexpected
 * (a proxy error page, say), or the request never arrived at all.
 */
export async function request(path, options = {}) {
  const { method = "GET", body, token, headers = {}, ...rest } = options;

  const requestHeaders = { ...headers };
  if (body !== undefined) {
    requestHeaders["Content-Type"] = "application/json";
  }
  if (token) {
    requestHeaders.Authorization = `Bearer ${token}`;
  }

  let response;
  try {
    response = await fetch(`${baseUrl()}${path}`, {
      method,
      headers: requestHeaders,
      body: body === undefined ? undefined : JSON.stringify(body),
      // Cookies matter for /auth/refresh and /auth/logout, which are
      // authenticated by the HttpOnly refresh cookie rather than a token.
      credentials: "include",
      ...rest,
    });
  } catch (cause) {
    // The API was unreachable — down, or DNS, or the wrong base URL. Worth
    // distinguishing from an API that answered with an error, because the fix
    // is completely different.
    throw new ApiError(0, "network_error", "Could not reach the server.", null);
  }

  if (response.status === 204) {
    return null;
  }

  // Parse defensively. A 502 from a proxy is HTML, and letting response.json()
  // throw would surface "Unexpected token <" instead of the status that
  // actually explains the failure.
  let payload = null;
  const text = await response.text();
  if (text) {
    try {
      payload = JSON.parse(text);
    } catch {
      payload = null;
    }
  }

  if (!response.ok) {
    const error = payload?.error;
    throw new ApiError(
      response.status,
      error?.code || "unexpected_error",
      error?.message || `Request failed with status ${response.status}.`,
      error?.fields,
    );
  }

  return payload;
}

/** The public storefront page for a creator. */
export function getPublicProfile(username) {
  return request(`/api/v1/public/${encodeURIComponent(username)}`);
}

/** One product's public page. */
export function getPublicProduct(username, slug) {
  return request(`/api/v1/public/${encodeURIComponent(username)}/${encodeURIComponent(slug)}`);
}

/**
 * Creates a pending order. Note what is not sent: an amount. The API prices the
 * order from its own database, and a client that tried to send a total would be
 * ignored — which is the entire reason a checkout can be public.
 */
export function createOrder({ buyerEmail, productIds, idempotencyKey }) {
  return request("/api/v1/orders", {
    method: "POST",
    body: { buyer_email: buyerEmail, product_ids: productIds },
    headers: idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {},
  });
}

/** Exchanges an order id and the buyer's email for short-lived download links. */
export function getDownloads(orderId, buyerEmail) {
  return request(`/api/v1/orders/${encodeURIComponent(orderId)}/download`, {
    method: "POST",
    body: { buyer_email: buyerEmail },
  });
}

/** Formats minor units for display. 49900 -> "₹499.00". */
export function formatMoney(minorUnits, currency = "INR") {
  // The API speaks paise end to end and this is the only place that divides by
  // 100 — at the moment of rendering, never before, so no arithmetic in this
  // app is ever done on a rounded value.
  return new Intl.NumberFormat("en-IN", {
    style: "currency",
    currency,
  }).format(minorUnits / 100);
}
