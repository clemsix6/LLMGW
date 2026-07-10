-- Seed notional list prices (USD per million tokens) for the GPT-5.6 Codex tiers. Cost is
-- notional because the operator's subscription is flat-rate; prices mirror OpenAI public list
-- tiers (Sol 5/30, Terra 2.50/15, Luna 1/6). ON CONFLICT keeps any hand-tuned price intact.
INSERT INTO model_price (model, input_usd_per_mtok, output_usd_per_mtok) VALUES
    ('gpt-5.6-sol',   5.00, 30.00),
    ('gpt-5.6-terra', 2.50, 15.00),
    ('gpt-5.6-luna',  1.00,  6.00)
ON CONFLICT (model) DO NOTHING;
