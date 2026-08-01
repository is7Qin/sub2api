-- Restore durable quota-exhaustion invalidations for billing rows staged before
-- migration 172 suppressed the generic API-key quota trigger.
INSERT INTO auth_cache_invalidation_outbox (cache_key, source_key)
SELECT DISTINCT
    encode(sha256(convert_to(k.key, 'UTF8')), 'hex'),
    'billing-quota:' || request_id || ':' || o.api_key_id::text
FROM billing_attempt_outbox AS o
JOIN api_keys AS k ON k.id = o.api_key_id
CROSS JOIN LATERAL (
    SELECT COALESCE(
        NULLIF(BTRIM(o.command ->> 'request_id'), ''),
        NULLIF(BTRIM(o.command -> 'billing' ->> 'request_id'), '')
    ) AS request_id
) AS identity
WHERE o.status IN ('finalization_pending', 'finalizing')
  AND o.apply_result @> '{"api_key_quota_exhausted": true}'::jsonb
  AND request_id IS NOT NULL
  AND k.deleted_at IS NULL
  AND k.key <> ''
ON CONFLICT (source_key) WHERE source_key IS NOT NULL DO NOTHING;
