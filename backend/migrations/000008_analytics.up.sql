-- A visitor is a pseudonym, not a person.
--
-- The identifier is sha256(ip + user agent + a secret salt + the UTC date),
-- and the date in that input is what makes this defensible. Two requests from
-- the same browser on the same day collide, so "unique visitors today" is
-- answerable; the same browser tomorrow hashes to something unrelated, so
-- building a profile of one person over weeks is not possible from this table
-- even by whoever holds the database.
--
-- The rejected alternatives, and why:
--
--   * Storing the raw IP. Simple, and a permanent record of who read what,
--     which is a liability that grows with the table.
--   * Hashing the IP alone. Reversible by brute force in minutes: there are
--     only about four billion IPv4 addresses, so a rainbow table is cheap.
--   * A cookie. Survives IP changes and is more accurate, but needs consent
--     banners in most of the world and can be cleared, so it is neither more
--     honest nor more reliable.
--
-- The cost is stated plainly: a visitor on a phone that switches from wifi to
-- mobile data counts twice, and two people behind one office NAT with the same
-- browser count once. These numbers are directional, not a census, and any
-- dashboard built on them should read "about 400 views", not "400 views".
CREATE TABLE visitors (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    profile_id   uuid        NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    -- The hash itself. 64 hex characters of sha256; stored as text rather than
    -- bytea because every consumer of it is a string comparison and nothing
    -- here does arithmetic on the bytes.
    visitor_hash text        NOT NULL,
    -- The UTC day the hash was computed for, stored separately so retention can
    -- delete by date without re-deriving anything, and so the daily rotation is
    -- visible in the data rather than implied by it.
    seen_on      date        NOT NULL,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),

    -- One row per visitor per creator per day. Scoped to the profile because a
    -- creator's analytics must not reveal that the same person also visited
    -- another creator: that correlation is exactly the tracking this design is
    -- meant to prevent.
    CONSTRAINT visitors_unique_per_day UNIQUE (profile_id, visitor_hash, seen_on),
    CONSTRAINT visitors_hash_shape CHECK (visitor_hash ~ '^[a-f0-9]{64}$'),
    CONSTRAINT visitors_seen_order CHECK (last_seen_at >= first_seen_at)
);

-- The creator dashboard asks "how many unique visitors in the last 30 days",
-- which is a range scan over this index with no heap access.
CREATE INDEX visitors_profile_day_idx ON visitors (profile_id, seen_on DESC);

-- Every interesting thing that happened, append-only.
--
-- One table with a type column rather than page_views + click_events + ... The
-- reason is the queries: a dashboard asks "views, clicks and purchases for this
-- creator this week", and across separate tables that is a UNION ALL of three
-- shapes that must be kept in step forever. The cost is that the target column
-- cannot simply be NOT NULL, since a link click has a link and a product view
-- has a product — so the consistency is enforced by a CHECK instead, below.
CREATE TABLE events (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    profile_id uuid        NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    -- Nullable: server-side events such as a purchase have no browser behind
    -- them, and inventing a visitor for one would corrupt the visitor counts.
    visitor_id uuid        REFERENCES visitors(id) ON DELETE SET NULL,
    event_type text        NOT NULL,

    -- Exactly one of these is set, according to event_type. ON DELETE CASCADE
    -- because analytics about a deleted link is noise; the aggregate counts a
    -- creator cares about are rebuilt from what still exists.
    product_id uuid        REFERENCES products(id) ON DELETE CASCADE,
    link_id    uuid        REFERENCES links(id)    ON DELETE CASCADE,
    -- Set only on purchase events, so revenue can be attributed without joining
    -- back to orders for every row on a dashboard.
    --
    -- CASCADE, not RESTRICT. An event is derived data about an order, so if the
    -- order is ever erased the derived row must go with it or the erasure is
    -- incomplete — which is exactly the situation a data-deletion request
    -- creates. RESTRICT would also make analytics the thing that blocks an
    -- erasure, which is precisely backwards.
    order_id   uuid        REFERENCES orders(id)   ON DELETE CASCADE,

    -- Where the visitor came from, host only. The full referrer URL can carry a
    -- search query or a private document title, so only the host is kept:
    -- "instagram.com" answers the question a creator actually has.
    referrer_host text,
    occurred_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT events_type_valid CHECK (
        event_type IN ('profile_view', 'product_view', 'link_click', 'purchase')
    ),
    -- Paired consistency, the same idea as orders.paid_at: the type and the
    -- target it implies are one fact stored twice, so the database enforces that
    -- they agree rather than trusting every future write path to remember.
    CONSTRAINT events_target_matches_type CHECK (
        (event_type = 'profile_view' AND product_id IS NULL AND link_id IS NULL AND order_id IS NULL)
     OR (event_type = 'product_view'  AND product_id IS NOT NULL AND link_id IS NULL AND order_id IS NULL)
     OR (event_type = 'link_click'    AND link_id IS NOT NULL AND product_id IS NULL AND order_id IS NULL)
     OR (event_type = 'purchase'      AND order_id IS NOT NULL AND product_id IS NOT NULL AND link_id IS NULL)
    ),
    CONSTRAINT events_referrer_len CHECK (referrer_host IS NULL OR char_length(referrer_host) <= 253)
);

-- The dashboard's main query: everything for this creator over a date range,
-- newest first. DESC in the index so that read is a backwards scan rather than
-- a sort of the whole range.
CREATE INDEX events_profile_time_idx ON events (profile_id, occurred_at DESC);

-- "How did this one product do?" Partial, because only product_view and
-- purchase rows carry a product, and indexing the NULLs would be dead weight in
-- a table where most rows are profile views.
CREATE INDEX events_product_idx ON events (product_id, occurred_at DESC)
    WHERE product_id IS NOT NULL;

CREATE INDEX events_link_idx ON events (link_id, occurred_at DESC)
    WHERE link_id IS NOT NULL;

-- Retention, and what is deliberately absent.
--
-- There is no partitioning here. This table grows fastest of any in the schema
-- and monthly partitions are the obvious answer, but at zero rows that is
-- machinery with no problem to solve, and it can be introduced later by
-- creating a partitioned table and moving the data. The trigger for doing it is
-- when deleting old rows starts to hurt, not a row count someone guessed.
--
-- A retention job belongs with the background workers in phase 11: raw events
-- older than 90 days get deleted, and the aggregates computed from them stay.
-- Until that job exists, this comment is the only thing saying the data was
-- meant to expire, which is worth being honest about.
