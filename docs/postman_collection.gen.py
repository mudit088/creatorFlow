"""Builds the Postman collection from the route table in cmd/api/routes.go.

Generated rather than hand-written so the JSON is valid by construction and the
collection can be regenerated when routes change.
"""
import json, os

BASE = "{{baseUrl}}"


def url(path, raw_query=None):
    """Postman wants the URL both as a string and pre-split into segments."""
    full = BASE + path
    parts = [p for p in path.strip("/").split("/") if p]
    u = {"raw": full, "host": [BASE], "path": parts}
    if raw_query:
        u["query"] = raw_query
    return u


def req(name, method, path, body=None, desc="", scripts=None, auth=None, headers=None,
        raw_url=None, file_src=None):
    request = {
        "method": method,
        "header": headers or ([{"key": "Content-Type", "value": "application/json"}] if body else []),
        # raw_url is for requests that do not go to our API at all - the presigned
        # S3 PUT already carries its own host, so prefixing baseUrl would produce
        # http://localhost:8080/http://localhost:9000/...
        "url": raw_url if raw_url else url(path),
        "description": desc,
    }
    if file_src:
        request["body"] = {"mode": "file", "file": {"src": file_src}}
    if body is not None:
        raw = json.dumps(body, indent=2).replace('"{{orderTotal}}"', "{{orderTotal}}")
        request["body"] = {"mode": "raw", "raw": raw,
                           "options": {"raw": {"language": "json"}}}
    if auth is not None:
        request["auth"] = auth

    item = {"name": name, "request": request, "response": []}
    if scripts:
        item["event"] = scripts
    return item


def test_script(lines):
    return [{"listen": "test", "script": {"type": "text/javascript", "exec": lines}}]


def prerequest_script(lines):
    return [{"listen": "prerequest", "script": {"type": "text/javascript", "exec": lines}}]


NO_AUTH = {"type": "noauth"}

# ---------------------------------------------------------------- probes
probes = [
    req("Health (liveness)", "GET", "/health", auth=NO_AUTH,
        desc="Checks nothing but the process. Deliberately does not touch the database: "
             "if it did, a brief Postgres blip would make an orchestrator kill healthy containers."),
    req("Ready (readiness)", "GET", "/ready", auth=NO_AUTH,
        desc="Postgres unreachable -> ready:false. Redis unreachable -> degraded but still ready, "
             "because the cache is not a source of truth."),
    req("Ping", "GET", "/api/v1/ping", auth=NO_AUTH,
        desc="Echoes the request id, so you can confirm the middleware chain is wired."),
]

# ------------------------------------------------------------------ auth
auth_items = [
    req("Register", "POST", "/api/v1/auth/register",
        body={"email": "creator{{$timestamp}}@example.com", "password": "CorrectHorse1!"},
        auth=NO_AUTH,
        desc="Creates an account and returns an access token. The refresh token is set as an "
             "HttpOnly cookie scoped to /api/v1/auth, so Postman's cookie jar handles it.",
        scripts=test_script([
            "const body = pm.response.json();",
            "if (body.access_token) {",
            "    pm.collectionVariables.set('accessToken', body.access_token);",
            "    pm.collectionVariables.set('userEmail', body.user ? body.user.email : '');",
            "}",
            "pm.test('registered', () => pm.expect(pm.response.code).to.be.oneOf([200, 201]));",
        ])),
    req("Login", "POST", "/api/v1/auth/login",
        body={"email": "{{userEmail}}", "password": "CorrectHorse1!"},
        auth=NO_AUTH,
        desc="Same error for 'no such user' and 'wrong password', plus a dummy hash on the "
             "not-found branch so both paths take the same time. Prevents account enumeration.",
        scripts=test_script([
            "const body = pm.response.json();",
            "if (body.access_token) { pm.collectionVariables.set('accessToken', body.access_token); }",
        ])),
    req("Refresh", "POST", "/api/v1/auth/refresh", auth=NO_AUTH,
        desc="Authenticated by the refresh cookie, not the access token - demanding a valid access "
             "token to refresh would defeat the point. Rotates the token: presenting a revoked one "
             "again revokes every session for that user.",
        scripts=test_script([
            "const body = pm.response.json();",
            "if (body.access_token) { pm.collectionVariables.set('accessToken', body.access_token); }",
        ])),
    req("Me", "GET", "/api/v1/me", desc="Requires a valid access token."),
    req("Logout", "POST", "/api/v1/auth/logout", auth=NO_AUTH,
        desc="Revokes the refresh token and clears the cookie."),
]

# -------------------------------------------------------------- profiles
profile_items = [
    req("Create profile", "POST", "/api/v1/profiles",
        body={"username": "creator{{$randomInt}}", "display_name": "Test Creator"},
        desc="One profile per user. The username is the public /@handle.",
        scripts=test_script([
            "const body = pm.response.json();",
            "if (body.id) {",
            "    pm.collectionVariables.set('profileId', body.id);",
            "    pm.collectionVariables.set('username', body.username);",
            "}",
        ])),
    req("Get my profile", "GET", "/api/v1/profiles/me",
        desc="Addresses /me rather than /profiles/:id: the id comes from the verified token and is "
             "never client-supplied, so 'forgot to check ownership' cannot be written."),
    req("Update profile", "POST", "/api/v1/profiles/me/update",
        body={"display_name": "Renamed Creator", "bio": "Fitness coach", "is_published": True},
        desc="Fields are pointers server-side, so an absent field and an explicit empty string "
             "are different things."),
    req("Add link", "POST", "/api/v1/profiles/me/links",
        body={"title": "YouTube", "url": "https://youtube.com/@example"},
        scripts=test_script([
            "const body = pm.response.json();",
            "if (body.id) { pm.collectionVariables.set('linkId', body.id); }",
        ])),
    req("Reorder links", "POST", "/api/v1/profiles/me/links/order",
        body={"link_ids": ["{{linkId}}"]},
        desc="Positions are enforced by a DEFERRABLE UNIQUE constraint, so a permutation is one "
             "UPDATE per row and the check happens at COMMIT."),
    req("Update link", "POST", "/api/v1/profiles/me/links/{{linkId}}/update",
        body={"title": "YouTube (main)", "is_active": True}),
    req("Delete link", "POST", "/api/v1/profiles/me/links/{{linkId}}/delete",
        desc="Returns 204. Not idempotent the way DELETE is - deleting twice gives 404."),
]

# -------------------------------------------------------------- products
product_items = [
    req("Create product", "POST", "/api/v1/products",
        body={"slug": "workout-plan-{{$randomInt}}", "title": "12 Week Plan",
              "description": "A plan", "price_minor": 49900},
        desc="price_minor is paise: 49900 = Rs 499. Never a float.",
        scripts=test_script([
            "const body = pm.response.json();",
            "if (body.id) {",
            "    pm.collectionVariables.set('productId', body.id);",
            "    pm.collectionVariables.set('productSlug', body.slug);",
            "}",
        ])),
    req("List products", "GET", "/api/v1/products"),
    req("Get product", "GET", "/api/v1/products/{{productId}}",
        desc="Scoped to the caller's profile in the same statement that fetches the row: someone "
             "else's product is reported as not found."),
    req("Request upload URL", "POST", "/api/v1/products/{{productId}}/upload-url",
        body={"filename": "plan.pdf", "content_type": "application/pdf", "size_bytes": 1024},
        desc="Returns a presigned S3/MinIO PUT URL. The signature covers Content-Type and "
             "Content-Length, so the client can only upload what it declared. Files never pass "
             "through the API.",
        scripts=test_script([
            "const body = pm.response.json();",
            "if (body.file) { pm.collectionVariables.set('fileId', body.file.id); }",
            "if (body.upload_url) { pm.collectionVariables.set('uploadUrl', body.upload_url); }",
        ])),
    req("Upload file to MinIO (direct PUT)", "PUT", "", auth=NO_AUTH,
        raw_url="{{uploadUrl}}", file_src="docs/sample-1024.pdf",
        headers=[{"key": "Content-Type", "value": "application/pdf"}],
        desc="NOT an API route - this goes straight to MinIO with the presigned URL from the "
             "previous request, which is why there is no Authorization header. The body is "
             "docs/sample-1024.pdf, exactly 1024 bytes, because the signature covers the "
             "Content-Length declared in the previous request: send a different size and S3 "
             "rejects it. In Postman, re-pick the file under Body -> binary if it shows as missing.",
        scripts=test_script([
            "pm.test('object stored', () => pm.expect(pm.response.code).to.be.oneOf([200, 204]));",
        ])),
    req("Confirm upload", "POST", "/api/v1/products/{{productId}}/files/{{fileId}}/confirm",
        desc="The API asks S3 whether the object exists and matches what was declared. Until this "
             "succeeds the file row has uploaded_at NULL and the product cannot be published."),
    req("Publish product", "POST", "/api/v1/products/{{productId}}/update",
        body={"status": "published"},
        desc="Refuses to publish a product with no confirmed file - the one invariant here that "
             "spans two tables and so cannot be a CHECK constraint."),
    req("Update product", "POST", "/api/v1/products/{{productId}}/update",
        body={"title": "12 Week Plan v2", "price_minor": 59900},
        desc="Changing the price does not alter existing orders: they carry snapshots."),
]

# ---------------------------------------------------------------- public
public_items = [
    req("Public profile page", "GET", "/api/v1/public/{{username}}", auth=NO_AUTH,
        desc="World-readable. Returns its own response types, separate from the owner-facing ones."),
    req("Public product page", "GET", "/api/v1/public/{{username}}/{{productSlug}}", auth=NO_AUTH),
]

# ---------------------------------------------------------------- orders
order_items = [
    req("Create order (checkout)", "POST", "/api/v1/orders",
        body={"buyer_email": "buyer@example.com", "product_ids": ["{{productId}}"]},
        auth=NO_AUTH,
        headers=[{"key": "Content-Type", "value": "application/json"},
                 {"key": "Idempotency-Key", "value": "{{$guid}}"}],
        desc="Public: buyers are guests. Note the body carries NO amount - the total is summed from "
             "the database. Send the same Idempotency-Key twice and you get 200 with the same order "
             "instead of a second one (replace {{$guid}} with a fixed string to try it).",
        scripts=test_script([
            "const body = pm.response.json();",
            "if (body.id) {",
            "    pm.collectionVariables.set('orderId', body.id);",
            "    pm.collectionVariables.set('orderTotal', body.total_minor);",
            "}",
            "if (body.checkout) {",
            "    pm.collectionVariables.set('providerOrderId', body.checkout.provider_order_id);",
            "    pm.collectionVariables.set('razorpayKeyId', body.checkout.key_id);",
            "} else {",
            "    console.log('no checkout block: RAZORPAY_KEY_ID/SECRET not configured');",
            "}",
        ])),
]

# -------------------------------------------------------------- payments
webhook_body = {
    "entity": "event",
    "event": "payment.captured",
    "contains": ["payment"],
    "payload": {"payment": {"entity": {
        "id": "pay_manual_{{$timestamp}}",
        "order_id": "{{providerOrderId}}",
        "amount": "{{orderTotal}}",
        "currency": "INR",
        "status": "captured",
    }}},
}

payment_items = [
    req("Webhook: payment.captured (signed)", "POST", "/api/v1/payments/webhook",
        body=webhook_body, auth=NO_AUTH,
        headers=[{"key": "Content-Type", "value": "application/json"},
                 {"key": "X-Razorpay-Signature", "value": "{{webhookSignature}}"},
                 {"key": "X-Razorpay-Event-Id", "value": "evt_{{$guid}}"}],
        desc="The pre-request script signs the resolved body with {{webhookSecret}}, exactly as "
             "Razorpay does: hex(HMAC-SHA256(rawBody, secret)). Set webhookSecret to the value in "
             ".env. Amount must equal the order total or nothing settles. Re-send with the same "
             "event id and it returns 200 having changed nothing.",
        scripts=test_script([
            "pm.test('accepted', () => pm.response.to.have.status(200));",
        ]) + prerequest_script([
            "// Reproduces Razorpay's signature: hex(HMAC-SHA256(raw body, webhook secret)).",
            "// The body is resolved first, because the bytes we sign must be the bytes we send.",
            "const secret = pm.environment.get('webhookSecret')",
            "    || pm.globals.get('webhookSecret')",
            "    || pm.collectionVariables.get('webhookSecret');",
            "if (!secret) {",
            "    console.warn('webhookSecret is not set - this request will get a 401');",
            "}",
            "const raw = pm.variables.replaceIn(pm.request.body.raw);",
            "pm.request.body.raw = raw;",
            "const sig = CryptoJS.HmacSHA256(raw, secret || '').toString(CryptoJS.enc.Hex);",
            "pm.collectionVariables.set('webhookSignature', sig);",
        ])),
    req("Webhook: forged signature (expect 401)", "POST", "/api/v1/payments/webhook",
        body=webhook_body, auth=NO_AUTH,
        headers=[{"key": "Content-Type", "value": "application/json"},
                 {"key": "X-Razorpay-Signature", "value": "deadbeef"},
                 {"key": "X-Razorpay-Event-Id", "value": "evt_forged_{{$guid}}"}],
        desc="Should return 401 and write nothing. This is the check that stops the endpoint being "
             "an unauthenticated 'mark my order paid' button.",
        scripts=test_script([
            "pm.test('forged delivery is rejected', () => pm.response.to.have.status(401));",
        ])),
]

# --------------------------------------------------------------- delivery
delivery_items = [
    req("Download (entitled buyer)", "POST", "/api/v1/orders/{{orderId}}/download",
        body={"buyer_email": "buyer@example.com"}, auth=NO_AUTH,
        desc="Returns a short-lived presigned S3 URL per file. The bytes never pass through the "
             "API - it decides who may have the file and then steps out of the way. Access is read "
             "from the entitlements table alone, which the webhook wrote in the same transaction "
             "that marked the order paid. Run the URL from the response in a browser: it "
             "downloads as the original filename, and expires after 5 minutes.",
        scripts=test_script([
            "pm.test('entitled', () => pm.response.to.have.status(200));",
            "const body = pm.response.json();",
            "if (body.files && body.files.length) {",
            "    pm.collectionVariables.set('downloadUrl', body.files[0].url);",
            "    pm.test('link expires quickly', () => pm.expect(body.files[0].expires_in).to.be.at.most(900));",
            "}",
        ])),
    req("Download (wrong email, expect 404)", "POST", "/api/v1/orders/{{orderId}}/download",
        body={"buyer_email": "attacker@evil.test"}, auth=NO_AUTH,
        desc="Holding the order id is not enough. Note the response is identical to the one for an "
             "order that does not exist - otherwise this endpoint would let anyone with a leaked "
             "order id confirm who bought it.",
        scripts=test_script([
            "pm.test('refused', () => pm.response.to.have.status(404));",
            "pm.test('reveals nothing', () => pm.expect(pm.response.json().error.code).to.eql('not_entitled'));",
        ])),
]

# -------------------------------------------------------------- dashboard
analytics_items = [
    req("Overview (last 30 days)", "GET", "/api/v1/analytics/overview",
        desc="The creator dashboard. Requires a token and takes no profile id: the queries scope "
             "to whoever the token says is calling, so asking for another creator's numbers is "
             "unexpressible rather than merely forbidden. Run this after the checkout and webhook "
             "folders and the purchase will be in it. Note daily_visitor_total is the sum of each "
             "day's unique visitors, not distinct people over the range - visitor hashes rotate at "
             "midnight so the second number cannot be computed.",
        scripts=test_script([
            "pm.test('dashboard returns', () => pm.response.to.have.status(200));",
            "const o = pm.response.json();",
            "pm.test('one row per day', () => pm.expect(o.days.length).to.be.above(0));",
            "pm.test('revenue is in minor units', () => pm.expect(o.revenue.revenue_minor).to.be.a('number'));",
        ])),
    req("Overview (explicit month)", "GET", "/api/v1/analytics/overview?from=2026-09-01&to=2026-10-01",
        desc="Half-open range: from is inclusive, to is exclusive. That is what lets September and "
             "October both be expressed without double-counting the 30th."),
    req("Overview (absurd range, expect 422)", "GET",
        "/api/v1/analytics/overview?from=1970-01-01&to=2026-12-31",
        desc="Bounded at 366 days. Without the limit, one authenticated caller could turn the "
             "dashboard into a repeatable full-table scan.",
        scripts=test_script([
            "pm.test('range is bounded', () => pm.response.to.have.status(422));",
        ])),
]

cleanup_items = [
    req("Delete a sold file (expect 409)", "POST",
        "/api/v1/products/{{productId}}/files/{{fileId}}/delete",
        desc="Runs last on purpose, and is expected to be REFUSED. By this point the buyer above "
             "has paid for this product and holds a live entitlement, so deleting the file would "
             "destroy a purchase someone paid for. The API refuses with 409 file_sold and tells "
             "the creator to archive the product instead. Delete the file BEFORE checkout (or "
             "revoke the entitlement) and the same request returns 204 - and if it was the last "
             "deliverable, the product drops back to draft so the storefront stops offering "
             "something checkout would refuse to sell.",
        scripts=test_script([
            "pm.test('sold file is protected', () => pm.response.to.have.status(409));",
            "pm.test('says why', () => pm.expect(pm.response.json().error.code).to.eql('file_sold'));",
        ])),
]

collection = {
    "info": {
        "name": "CreatorFlow API (local)",
        "description": (
            "Every route the Go API serves, in the order you would exercise them: register -> "
            "profile -> product -> upload -> publish -> public page -> checkout -> webhook.\n\n"
            "Set `baseUrl` (default http://localhost:8080) and, for the webhook requests, "
            "`webhookSecret` to the RAZORPAY_WEBHOOK_SECRET from .env.\n\n"
            "Tokens chain automatically: Register/Login store the access token in a collection "
            "variable that every authenticated request uses, and Create product / Add link / "
            "Create order store their ids for the requests that follow.\n\n"
            "The API speaks GET and POST only - state changes are POSTs whose path names the action."
        ),
        "schema": "https://schema.getpostman.com/json/collection/v2.1.0/collection.json",
    },
    "auth": {"type": "bearer", "bearer": [{"key": "token", "value": "{{accessToken}}", "type": "string"}]},
    "variable": [
        {"key": "baseUrl", "value": "http://localhost:8080"},
        {"key": "accessToken", "value": ""},
        {"key": "userEmail", "value": ""},
        {"key": "profileId", "value": ""},
        {"key": "username", "value": ""},
        {"key": "productId", "value": ""},
        {"key": "productSlug", "value": ""},
        {"key": "fileId", "value": ""},
        {"key": "uploadUrl", "value": ""},
        {"key": "linkId", "value": ""},
        {"key": "orderId", "value": ""},
        {"key": "orderTotal", "value": ""},
        {"key": "downloadUrl", "value": ""},
        {"key": "providerOrderId", "value": ""},
        {"key": "razorpayKeyId", "value": ""},
        {"key": "webhookSecret", "value": "", "type": "string"},
        {"key": "webhookSignature", "value": ""},
    ],
    "item": [
        {"name": "0. Probes", "item": probes},
        {"name": "1. Auth", "item": auth_items},
        {"name": "2. Profile & links", "item": profile_items},
        {"name": "3. Products & files", "item": product_items},
        {"name": "4. Public storefront", "item": public_items},
        {"name": "5. Checkout", "item": order_items},
        {"name": "6. Payments webhook", "item": payment_items},
        {"name": "7. Download", "item": delivery_items},
        {"name": "8. Creator dashboard", "item": analytics_items},
        {"name": "9. Cleanup (run last)", "item": cleanup_items},
    ],
}

out = os.path.join("M:\\", "creatorFlow", "docs", "creatorflow.postman_collection.json")
os.makedirs(os.path.dirname(out), exist_ok=True)
with open(out, "w", encoding="utf-8") as f:
    json.dump(collection, f, indent=2)

count = sum(len(folder["item"]) for folder in collection["item"])
print(f"wrote {out} with {count} requests in {len(collection['item'])} folders")
