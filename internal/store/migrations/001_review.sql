-- A review session: one person and one agent over one HTML file, keyed by the
-- canonical file path's hash. status is 'open' (nothing pending), 'feedback'
-- (sent feedback or a fatal artifact failure waits for the agent's poll) or
-- 'ended'. ended_by records who ended it: a session the user ended never
-- reopens without the agent's explicit --reopen.
CREATE TABLE sessions (
    key               TEXT PRIMARY KEY,
    file              TEXT NOT NULL UNIQUE,
    status            TEXT NOT NULL DEFAULT 'open',
    ended_by          TEXT NOT NULL DEFAULT '',
    artifact_revision INTEGER NOT NULL DEFAULT 0,
    dom_snapshot      TEXT NOT NULL DEFAULT '',
    artifact_failures TEXT NOT NULL DEFAULT '[]',
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
);

-- Notes the reviewer queued but has not sent yet. Held on the server so every
-- open tab shows the same queue and nothing is lost on reload. kind is
-- 'annotation', 'answer' (queued by the artifact through queuePrompt) or
-- 'message'. Items sharing a non-empty queue_key replace each other.
CREATE TABLE queue_items (
    id          INTEGER PRIMARY KEY,
    session_key TEXT NOT NULL REFERENCES sessions(key) ON DELETE CASCADE,
    kind        TEXT NOT NULL,
    prompt      TEXT NOT NULL DEFAULT '',
    selector    TEXT NOT NULL DEFAULT '',
    tag         TEXT NOT NULL DEFAULT '',
    excerpt     TEXT NOT NULL DEFAULT '',
    target      TEXT NOT NULL DEFAULT '',
    queue_key   TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL
);
CREATE INDEX queue_items_session ON queue_items(session_key);

-- Sent feedback in the shape the agent receives it. delivered_at is set when a
-- poll takes it and cleared again if that poll's response could not be
-- written, so the next poll delivers it.
CREATE TABLE prompts (
    id           INTEGER PRIMARY KEY,
    session_key  TEXT NOT NULL REFERENCES sessions(key) ON DELETE CASCADE,
    payload      TEXT NOT NULL,
    delivered_at INTEGER,
    created_at   INTEGER NOT NULL
);
CREATE INDEX prompts_pending ON prompts(session_key, delivered_at);

-- The conversation: what the reviewer sent and what the agent replied.
CREATE TABLE chat_entries (
    id          INTEGER PRIMARY KEY,
    session_key TEXT NOT NULL REFERENCES sessions(key) ON DELETE CASCADE,
    role        TEXT NOT NULL,
    kind        TEXT NOT NULL,
    text        TEXT NOT NULL DEFAULT '',
    anchor      TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL
);
CREATE INDEX chat_entries_session ON chat_entries(session_key);
