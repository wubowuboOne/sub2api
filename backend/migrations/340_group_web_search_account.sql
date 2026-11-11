ALTER TABLE groups ADD COLUMN IF NOT EXISTS web_search_account_id BIGINT NOT NULL DEFAULT 0;
COMMENT ON COLUMN groups.web_search_account_id IS 'Dedicated OpenAI standalone web search account; 0 preserves normal scheduling';
