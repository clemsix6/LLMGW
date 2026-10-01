-- Seeds claude-sonnet-5-5 at Anthropic's published list prices (USD per
-- million tokens): https://platform.claude.com/docs/en/about-claude/pricing.
--
-- The registry lists the model under this undated id directly, but as with
-- every Claude model already seeded, a served request can still be reported
-- under a dated sibling, so both patterns are seeded together the way 0021
-- did for the model before it. Cache-write is the 5-minute rate, which follows
-- the 1.25x-input convention 0013 established for Claude models; cache-read is
-- Anthropic's published rate for this model. ON CONFLICT keeps any hand-tuned
-- price an operator already set.
INSERT INTO model_price (
    provider, model_pattern, service_tier,
    input_per_million, output_per_million,
    cache_read_per_million, cache_creation_per_million,
    effective_from
)
VALUES
    ('*', 'claude-sonnet-5-5',   '*', 2, 10, 0.20, 2.50, '1970-01-01T00:00:00Z'::timestamptz),
    ('*', 'claude-sonnet-5-5-*', '*', 2, 10, 0.20, 2.50, '1970-01-01T00:00:00Z'::timestamptz)
ON CONFLICT (provider, model_pattern, service_tier, effective_from) DO NOTHING;
