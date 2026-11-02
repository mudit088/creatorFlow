/** @type {import('next').NextConfig} */
const nextConfig = {
  // Required by the prod target in Dockerfile, which copies .next/standalone
  // and runs server.js. Without it that COPY fails and the image cannot build.
  output: "standalone",

  // Hides the dev-only overlay button. It is useful while developing but it
  // sits in the corner of every screenshot taken for design review.
  devIndicators: false,

  // Deliberately NOT enabling cacheComponents / "use cache" for storefront
  // pages. The Go API already caches them in Redis for five minutes and drops
  // the entry when a creator edits their page. A second cache here would have
  // its own expiry that the API's invalidation cannot reach, so an edit would
  // clear Redis while Next kept serving the old HTML — stale in a way that
  // looks like a bug and cannot be fixed from the backend.
  //
  // Next 16 does not cache fetch by default, so leaving this off means every
  // request asks the API, and the API answers from Redis. One cache, one owner.
};

export default nextConfig;
