CREATE TABLE products (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    profile_id   uuid        NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    slug         citext      NOT NULL,
    title        text        NOT NULL,
    description  text,
    -- Money is a bigint of minor units. 499 rupees is 49900 paise. Never float:
    -- 0.1 + 0.2 is not 0.3 in binary floating point, and a rounding error in a
    -- ledger is not a rounding error, it is a wrong number someone is owed.
    price_minor  bigint      NOT NULL,
    currency     char(3)     NOT NULL DEFAULT 'INR',
    status       text        NOT NULL DEFAULT 'draft',
    published_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),

    -- Unique per creator, not globally. Two creators should both be able to sell
    -- a "workout-plan"; a global unique would make slugs a land grab and leak
    -- how many products exist. The public URL is /@username/slug, so the pair is
    -- what has to be unique for the route to resolve.
    CONSTRAINT products_slug_unique UNIQUE (profile_id, slug),
    CONSTRAINT products_slug_shape  CHECK (slug ~ '^[a-z0-9]([a-z0-9-]{1,58}[a-z0-9])?$'),
    CONSTRAINT products_title_len   CHECK (char_length(title) BETWEEN 1 AND 120),
    -- Free products are allowed; negative ones are not.
    CONSTRAINT products_price_nonneg CHECK (price_minor >= 0),
    -- A sanity ceiling. It exists to catch a client that sends rupees where the
    -- API expects paise, which otherwise silently prices a product 100x wrong.
    CONSTRAINT products_price_max    CHECK (price_minor <= 100000000),
    CONSTRAINT products_currency_valid CHECK (currency = 'INR'),
    CONSTRAINT products_status_valid   CHECK (status IN ('draft', 'published', 'archived')),
    -- Paired consistency: published_at is set exactly when status is published.
    -- Application code that forgets one half of a state change is the norm, not
    -- the exception, so the pair is enforced here instead.
    CONSTRAINT products_published_consistent
        CHECK ((status = 'published') = (published_at IS NOT NULL))
);

CREATE TRIGGER products_set_updated_at
    BEFORE UPDATE ON products
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Partial: the public storefront only ever reads published rows, so drafts and
-- archived products have no business taking up space in the index that serves
-- the hot path.
CREATE INDEX products_profile_published_idx
    ON products (profile_id)
    WHERE status = 'published';

CREATE TABLE product_files (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    product_id    uuid        NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    -- The object key in the bucket. UNIQUE because two rows pointing at one
    -- object means deleting either row orphans or destroys the other's file.
    s3_key        text        NOT NULL UNIQUE,
    -- What the creator called the file. Serving the raw key as a filename would
    -- hand the buyer a uuid; this is what goes in Content-Disposition.
    original_name text        NOT NULL,
    content_type  text        NOT NULL,
    size_bytes    bigint      NOT NULL,
    checksum      text,
    -- NULL until the browser's direct-to-S3 upload is confirmed. A row exists
    -- from the moment a presigned URL is issued, so an abandoned upload is
    -- visible and collectable rather than an invisible orphan object.
    uploaded_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT product_files_size_positive CHECK (size_bytes > 0),
    -- 5 GB, the largest object a single PUT can carry. Anything above this needs
    -- multipart upload, which is a different flow, so the constraint marks the
    -- boundary rather than letting a 6 GB request fail confusingly at S3.
    CONSTRAINT product_files_size_max CHECK (size_bytes <= 5368709120),
    CONSTRAINT product_files_name_len CHECK (char_length(original_name) BETWEEN 1 AND 255)
);

CREATE TRIGGER product_files_set_updated_at
    BEFORE UPDATE ON product_files
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX product_files_product_idx ON product_files (product_id);

-- Finds abandoned uploads: rows whose presigned URL was issued but never
-- confirmed. Partial, because the cleanup job is the only reader and it only
-- ever wants the unconfirmed ones.
CREATE INDEX product_files_pending_idx
    ON product_files (created_at)
    WHERE uploaded_at IS NULL;
