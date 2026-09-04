-- Repairs the seeded price of claude-haiku-4-5, which carried Haiku 3.5 rates.
--
-- Migration 0003 seeded the model with the published rates of the previous
-- generation, so every attempt was costed at 0.8x its real price. The error
-- then spread on its own: 0011 duplicated the row onto the dated
-- "claude-haiku-4-5-*" pattern, and 0013 derived both cache rates from the
-- wrong input. All four rates on both patterns are wrong, by the same factor.
-- The values set below are Anthropic's published Haiku 4.5 list prices
-- (https://docs.claude.com/en/docs/about-claude/pricing).
--
-- The guard matches the faulty input/output pair rather than the model name.
-- An operator who deliberately priced Haiku otherwise -- or who already
-- applied this correction by hand to a running database -- carries a
-- different pair, so the statement passes over the row and reports zero rows
-- updated. Cache rates stay out of the guard: while the base pair is still
-- the faulty seed, whatever sits in the cache columns was derived from it and
-- is wrong too.
--
-- Migration 0003 is deliberately left as it stands. Applied migrations are
-- recorded by name and never replayed, so editing it would repair nothing on
-- an existing database while making the file disagree with what those
-- databases actually ran. A database created from scratch replays 0003, then
-- 0011, 0013 and this statement, and ends at the correct rates.
UPDATE model_price
SET input_per_million          = 1,
    output_per_million         = 5,
    cache_read_per_million     = 0.1,
    cache_creation_per_million = 1.25
WHERE model_pattern IN ('claude-haiku-4-5', 'claude-haiku-4-5-*')
  AND input_per_million = 0.80::double precision
  AND output_per_million = 4::double precision;
