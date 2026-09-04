-- Repairs the seeded price of claude-opus-4-8, which carried Opus 4.1 rates.
--
-- Migration 0003 seeded the model at 15 / 75 per million, the published rates
-- of the retired Opus 4.1, so every attempt was costed at three times its
-- real price. As with claude-haiku-4-5 (0019), the error then spread on its
-- own: 0011 duplicated the row onto the dated "claude-opus-4-8-*" pattern and
-- 0013 derived both cache rates from the wrong input. The values set below
-- are Anthropic's published Opus 4.8 list prices
-- (https://docs.claude.com/en/docs/about-claude/pricing): 5 / 25, cache read
-- at a tenth of input, cache creation at 1.25 times input.
--
-- The guard is the full identity of the rows the seed produced -- wildcard
-- provider and service tier, the epoch effective date 0010 imports them with
-- -- narrowed to the faulty rates themselves, so a price an operator wrote and
-- a row already carrying the correction are both passed over. Cache rates stay
-- out of the guard: while the base pair is still the faulty seed, whatever
-- sits in the cache columns was derived from it and is wrong too.
--
-- Migration 0003 is deliberately left as it stands, for the reason 0019 gives.
UPDATE model_price
SET input_per_million          = 5,
    output_per_million         = 25,
    cache_read_per_million     = 0.5,
    cache_creation_per_million = 6.25
WHERE model_pattern IN ('claude-opus-4-8', 'claude-opus-4-8-*')
  AND provider = '*'
  AND service_tier = '*'
  AND effective_from = '1970-01-01T00:00:00Z'::timestamptz
  AND input_per_million = 15::double precision
  AND output_per_million = 75::double precision;
