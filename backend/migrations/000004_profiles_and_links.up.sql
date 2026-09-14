CREATE TABLE profiles (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid        NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
    username     citext      NOT NULL UNIQUE,
    display_name text        NOT NULL,
    bio          text,
    avatar_key   text,
    is_published boolean     NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT profiles_username_shape
        CHECK (username ~ '^[a-z0-9_]{3,30}$'),
    CONSTRAINT profiles_username_reserved
        CHECK (username NOT IN ('api', 'admin', 'login', 'signup', 'dashboard',
                                'settings', 'about', 'pricing', 'support'))
);

CREATE TRIGGER profiles_set_updated_at
    BEFORE UPDATE ON profiles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE links (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    profile_id uuid        NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    title      text        NOT NULL,
    url        text        NOT NULL,
    position   integer     NOT NULL,
    is_active  boolean     NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT links_title_len CHECK (char_length(title) BETWEEN 1 AND 80),
    CONSTRAINT links_url_scheme CHECK (url ~* '^https?://')
);

CREATE TRIGGER links_set_updated_at
    BEFORE UPDATE ON links
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX links_profile_position_idx ON links (profile_id, position);