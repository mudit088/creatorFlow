-- A link's position is an ordering invariant, so the database should be the one
-- enforcing it. Without this, a reorder that half-succeeds leaves two links
-- sharing position 3 and ORDER BY position returns them in whatever order the
-- executor feels like that day — a bug that reproduces only under load.
--
-- DEFERRABLE INITIALLY DEFERRED is the whole point. Any genuine reorder passes
-- through states where two rows briefly share a position: swapping 1 and 2 must
-- set one of them before the other. An immediate constraint rejects that first
-- UPDATE, forcing the classic workaround of parking every row at position+1000
-- and writing each row twice. Deferring the check to COMMIT means Postgres
-- verifies the final state only, so a permutation is one UPDATE per row and
-- either the entire new order is valid or the transaction rolls back.
--
-- The cost, stated plainly: a deferrable constraint cannot back an
-- INSERT ... ON CONFLICT (profile_id, position) clause. Nothing here needs one,
-- and append-time collisions are prevented by locking the profile row instead.
ALTER TABLE links
    ADD CONSTRAINT links_profile_position_unique
    UNIQUE (profile_id, position)
    DEFERRABLE INITIALLY DEFERRED;

-- Now redundant: the constraint above creates its own btree on exactly
-- (profile_id, position). Keeping both would mean every insert, delete and
-- reorder writes two identical indexes for no read benefit.
DROP INDEX IF EXISTS links_profile_position_idx;
