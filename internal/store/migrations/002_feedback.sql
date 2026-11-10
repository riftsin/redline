-- Delivery receipts on what the reviewer sent: Seen when a poll takes it,
-- Working when the artifact changes after that, Done when the agent replies.
-- receipt is '' (live), 'closed' (frozen when its review round ended) or
-- 'none' (sent before receipts existed: nothing was observed, show nothing).
ALTER TABLE chat_entries ADD COLUMN delivered_at INTEGER;
ALTER TABLE chat_entries ADD COLUMN working_at INTEGER;
ALTER TABLE chat_entries ADD COLUMN done_at INTEGER;
ALTER TABLE chat_entries ADD COLUMN receipt TEXT NOT NULL DEFAULT '';
UPDATE chat_entries SET receipt = 'none' WHERE role = 'user';

-- Which conversation entry a sent prompt came from, so delivery stamps land
-- on the right bubble.
ALTER TABLE prompts ADD COLUMN chat_entry_id INTEGER;

-- Images attached to a note or message: [{"id": ..., "name": ...}].
ALTER TABLE queue_items ADD COLUMN attachments TEXT NOT NULL DEFAULT '[]';
ALTER TABLE chat_entries ADD COLUMN attachments TEXT NOT NULL DEFAULT '[]';

-- Uploaded images, stored content-addressed on disk under the state
-- directory; id is the SHA-256 of the bytes.
CREATE TABLE attachments (
    id         TEXT PRIMARY KEY,
    mime       TEXT NOT NULL,
    bytes      INTEGER NOT NULL,
    width      INTEGER NOT NULL,
    height     INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    used_at    INTEGER NOT NULL
);

-- What changed in each artifact revision, as the review page measured it:
-- the innermost changed blocks, or a whole-page rewrite.
CREATE TABLE changes (
    session_key TEXT NOT NULL REFERENCES sessions(key) ON DELETE CASCADE,
    revision    INTEGER NOT NULL,
    selectors   TEXT NOT NULL DEFAULT '[]',
    rewrite     INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (session_key, revision)
);
