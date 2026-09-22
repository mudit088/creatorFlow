CREATE INDEX IF NOT EXISTS links_profile_position_idx ON links (profile_id, position);

ALTER TABLE links
    DROP CONSTRAINT IF EXISTS links_profile_position_unique;
