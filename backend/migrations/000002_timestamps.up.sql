-- Keeps updated_at honest. Doing this in application code means every INSERT
-- path has to remember it, and one day one of them won't.
CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;