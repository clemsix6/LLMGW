-- Seed model_price with notional list prices (USD per MILLION tokens) for the models LLMGW
-- currently routes, at standard Anthropic list-price tiers. These are operator-editable config
-- defaults: edit the rows directly to re-value Claude Max usage or to add new models.
-- ON CONFLICT keeps any hand-tuned price intact on re-apply.
INSERT INTO model_price (model, input_usd_per_mtok, output_usd_per_mtok) VALUES
    ('claude-opus-4-8',   15,   75),
    ('claude-sonnet-4-6',  3,   15),
    ('claude-haiku-4-5',   0.80, 4)
ON CONFLICT (model) DO NOTHING;
