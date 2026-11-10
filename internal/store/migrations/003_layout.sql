-- The passive layout-issues inbox: one JSON array of warning records per
-- session, folded by internal/layout from the review page's audit passes.
ALTER TABLE sessions ADD COLUMN layout_warnings TEXT NOT NULL DEFAULT '[]';
