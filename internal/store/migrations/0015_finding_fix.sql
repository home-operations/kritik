-- A finding may cover a range of lines (end_line, 0 for its line alone),
-- carry a replacement for those lines that the forge offers as a one-click
-- suggestion, and a prompt a coding agent applies the fix from.
ALTER TABLE findings ADD COLUMN end_line     int  NOT NULL DEFAULT 0;
ALTER TABLE findings ADD COLUMN replacement  text NOT NULL DEFAULT '';
ALTER TABLE findings ADD COLUMN agent_prompt text NOT NULL DEFAULT '';
