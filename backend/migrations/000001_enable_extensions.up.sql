-- Case-insensitive text: emails and usernames must not be case-sensitive.
CREATE EXTENSION IF NOT EXISTS citext;

-- pgcrypto gives us gen_random_uuid() for primary keys.
CREATE EXTENSION IF NOT EXISTS pgcrypto;
