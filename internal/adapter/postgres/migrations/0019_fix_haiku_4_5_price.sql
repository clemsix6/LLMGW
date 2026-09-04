-- Repairs the seeded price of claude-haiku-4-5, which carried Haiku 3.5 rates.
--
-- Migration 0003 seeded the model with the published rates of the previous
-- generation, so every attempt was costed under its real price. The error
-- then spread on its own: 0011 duplicated the row onto the dated
-- "claude-haiku-4-5-*" pattern, and 0013 derived both cache rates from the
-- wrong input. All four rates on both patterns are wrong, by the same factor.
-- The values set below are Anthropic's published Haiku 4.5 list prices
-- (https://docs.claude.com/en/docs/about-claude/pricing).
--
-- The guard is the full identity of the rows the seed produced -- the wildcard
-- provider and service tier and the epoch effective date 0010 imports them
-- with -- narrowed to the faulty rates themselves. A price an operator wrote
-- differs in at least one of those, and so does a row already carrying the
-- correction, so the statement passes over it and reports zero rows updated.
-- Cache rates stay out of the guard: while the base pair is still the faulty
-- seed, whatever sits in the cache columns was derived from it and is wrong
-- too.
--
-- Migration 0003 is deliberately left as it stands. Applied migrations are
-- recorded by name and never replayed, so editing it would repair nothing on
-- an existing database while making the file disagree with what those
-- databases actually ran. A database created from scratch replays the seed and
-- everything derived from it, this statement last, and ends at the right rates.
UPDATE model_price
SET input_per_million          = 1,
    output_per_million         = 5,
    cache_read_per_million     = 0.1,
    cache_creation_per_million = 1.25
WHERE model_pattern IN ('claude-haiku-4-5', 'claude-haiku-4-5-*')
  AND provider = '*'
  AND service_tier = '*'
  AND effective_from = '1970-01-01T00:00:00Z'::timestamptz
  AND input_per_million = 0.80::double precision
  AND output_per_million = 4::double precision;
