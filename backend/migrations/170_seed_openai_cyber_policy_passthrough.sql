-- Seed a low-precedence, administrator-editable fallback for OpenAI cyber-policy failures.
INSERT INTO error_passthrough_rules (
    name,
    enabled,
    priority,
    error_codes,
    keywords,
    match_mode,
    platforms,
    passthrough_code,
    response_code,
    passthrough_body,
    custom_message,
    skip_monitoring,
    description
)
SELECT
    'OpenAI cyber_policy fallback',
    TRUE, 1000,
    '[]'::jsonb, '["cyber_policy"]'::jsonb, 'all', '["openai"]'::jsonb,
    FALSE, 400, TRUE,
    NULL,
    FALSE,
    'Returns OpenAI cyber_policy semantic failures as HTTP 400 while preserving the sanitized upstream message.'
WHERE NOT EXISTS (
    SELECT 1
    FROM error_passthrough_rules
    WHERE name = 'OpenAI cyber_policy fallback'
);
