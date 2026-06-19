-- Seed notional list prices (USD per million tokens) for ChatGPT Codex models. Cost is
-- notional because the operator's subscription is flat-rate; prices approximate OpenAI
-- public list tiers. ON CONFLICT keeps any hand-tuned price intact on re-apply.
INSERT INTO model_price (model, input_usd_per_mtok, output_usd_per_mtok) VALUES
    ('gpt-5',       1.25, 10.00),
    ('gpt-5-codex', 1.25, 10.00),
    ('gpt-5.5',     1.25, 10.00)
ON CONFLICT (model) DO NOTHING;
