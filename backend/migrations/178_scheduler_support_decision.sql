CREATE SEQUENCE IF NOT EXISTS scheduler_support_publication_generation_seq
    AS BIGINT
    INCREMENT BY 1
    MINVALUE 1
    START WITH 1
    NO CYCLE;

-- Support-decision work is coalesced by group. When a changed channel has no
-- membership, the publisher cannot derive a bounded scope and needs a global row.
CREATE OR REPLACE FUNCTION scheduler_support_dirty_channels(channel_ids bigint[])
RETURNS void
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
BEGIN
    INSERT INTO public.scheduler_dirty_group_sources (group_id)
    SELECT DISTINCT cg.group_id
    FROM public.channel_groups cg
    WHERE cg.channel_id = ANY(channel_ids)
    ORDER BY cg.group_id
    ON CONFLICT (group_id) DO UPDATE
    SET generation = public.scheduler_dirty_group_sources.generation + 1,
        updated_at = statement_timestamp();

    INSERT INTO public.scheduler_dirty_work (kind, entity_id, rebuild_buckets)
    SELECT 3, 0, true
    WHERE EXISTS (
        SELECT 1
        FROM unnest(channel_ids) AS changed(channel_id)
        WHERE NOT EXISTS (
            SELECT 1 FROM public.channel_groups cg
            WHERE cg.channel_id = changed.channel_id
        )
    )
    ON CONFLICT (kind, entity_id) DO UPDATE
    SET generation = public.scheduler_dirty_work.generation + 1,
        rebuild_buckets = true,
        failure_count = 0,
        last_failure_at = NULL,
        last_error = '',
        retry_at = clock_timestamp(),
        updated_at = clock_timestamp();
END
$$;

CREATE OR REPLACE FUNCTION scheduler_support_channels_from_new()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
DECLARE
    channel_ids bigint[];
BEGIN
    SELECT array_agg(id ORDER BY id) INTO channel_ids FROM new_rows;
    IF channel_ids IS NOT NULL THEN
        PERFORM public.scheduler_support_dirty_channels(channel_ids);
    END IF;
    RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION scheduler_support_channels_dirty()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
DECLARE
    channel_ids bigint[];
BEGIN
    SELECT array_agg(affected.channel_id ORDER BY affected.channel_id)
    INTO channel_ids
    FROM (
        SELECT COALESCE(n.id, o.id) AS channel_id
        FROM old_rows AS o FULL JOIN new_rows AS n USING (id)
        WHERE o.id IS NULL OR n.id IS NULL OR
              o.status IS DISTINCT FROM n.status OR
              o.restrict_models IS DISTINCT FROM n.restrict_models OR
              o.billing_model_source IS DISTINCT FROM n.billing_model_source
    ) AS affected;

    IF channel_ids IS NOT NULL THEN
        PERFORM public.scheduler_support_dirty_channels(channel_ids);
    END IF;
    RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION scheduler_support_channels_from_old()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
DECLARE
    channel_ids bigint[];
BEGIN
    SELECT array_agg(id ORDER BY id) INTO channel_ids FROM old_rows;
    IF channel_ids IS NOT NULL THEN
        PERFORM public.scheduler_support_dirty_channels(channel_ids);
    END IF;
    RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION scheduler_support_channel_groups_from_new()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
BEGIN
    INSERT INTO public.scheduler_dirty_group_sources (group_id)
    SELECT group_id FROM new_rows ORDER BY group_id
    ON CONFLICT (group_id) DO UPDATE
    SET generation = public.scheduler_dirty_group_sources.generation + 1,
        updated_at = statement_timestamp();
    RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION scheduler_support_channel_groups_dirty()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
BEGIN
    INSERT INTO public.scheduler_dirty_group_sources (group_id)
    WITH changed AS MATERIALIZED (
        SELECT o.group_id AS old_group_id, n.group_id AS new_group_id
        FROM old_rows AS o JOIN new_rows AS n USING (id)
        WHERE o.channel_id IS DISTINCT FROM n.channel_id OR
              o.group_id IS DISTINCT FROM n.group_id
    )
    SELECT group_id
    FROM (
        SELECT old_group_id AS group_id FROM changed
        UNION
        SELECT new_group_id AS group_id FROM changed
    ) AS affected
    ORDER BY group_id
    ON CONFLICT (group_id) DO UPDATE
    SET generation = public.scheduler_dirty_group_sources.generation + 1,
        updated_at = statement_timestamp();
    RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION scheduler_support_channel_groups_from_old()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
BEGIN
    INSERT INTO public.scheduler_dirty_group_sources (group_id)
    SELECT group_id FROM old_rows ORDER BY group_id
    ON CONFLICT (group_id) DO UPDATE
    SET generation = public.scheduler_dirty_group_sources.generation + 1,
        updated_at = statement_timestamp();
    RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION scheduler_support_dirty_pricing_channels(channel_ids bigint[])
RETURNS void
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
DECLARE
    support_channel_ids bigint[];
BEGIN
    SELECT array_agg(affected.channel_id ORDER BY affected.channel_id)
    INTO support_channel_ids
    FROM (
        SELECT DISTINCT changed.channel_id
        FROM unnest(channel_ids) AS changed(channel_id)
        JOIN public.channels c ON c.id = changed.channel_id
        WHERE c.status = 'active'
          AND c.restrict_models
          AND c.billing_model_source = 'upstream'
    ) AS affected;

    IF support_channel_ids IS NOT NULL THEN
        PERFORM public.scheduler_support_dirty_channels(support_channel_ids);
    END IF;
END
$$;

CREATE OR REPLACE FUNCTION scheduler_support_channel_pricing_from_new()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
DECLARE
    channel_ids bigint[];
BEGIN
    SELECT array_agg(DISTINCT channel_id ORDER BY channel_id) INTO channel_ids FROM new_rows;
    IF channel_ids IS NOT NULL THEN
        PERFORM public.scheduler_support_dirty_pricing_channels(channel_ids);
    END IF;
    RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION scheduler_support_channel_pricing_dirty()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
DECLARE
    channel_ids bigint[];
BEGIN
    WITH changed AS MATERIALIZED (
        SELECT o.channel_id AS old_channel_id, n.channel_id AS new_channel_id
        FROM old_rows AS o JOIN new_rows AS n USING (id)
        WHERE o.channel_id IS DISTINCT FROM n.channel_id OR
              o.models IS DISTINCT FROM n.models OR
              o.platform IS DISTINCT FROM n.platform
    )
    SELECT array_agg(affected.channel_id ORDER BY affected.channel_id)
    INTO channel_ids
    FROM (
        SELECT old_channel_id AS channel_id FROM changed
        UNION
        SELECT new_channel_id AS channel_id FROM changed
    ) AS affected;

    IF channel_ids IS NOT NULL THEN
        PERFORM public.scheduler_support_dirty_pricing_channels(channel_ids);
    END IF;
    RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION scheduler_support_channel_pricing_from_old()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
DECLARE
    channel_ids bigint[];
BEGIN
    SELECT array_agg(DISTINCT channel_id ORDER BY channel_id) INTO channel_ids FROM old_rows;
    IF channel_ids IS NOT NULL THEN
        PERFORM public.scheduler_support_dirty_pricing_channels(channel_ids);
    END IF;
    RETURN NULL;
END
$$;

REVOKE ALL ON FUNCTION scheduler_support_dirty_channels(bigint[]) FROM PUBLIC;
REVOKE ALL ON FUNCTION scheduler_support_channels_from_new() FROM PUBLIC;
REVOKE ALL ON FUNCTION scheduler_support_channels_dirty() FROM PUBLIC;
REVOKE ALL ON FUNCTION scheduler_support_channels_from_old() FROM PUBLIC;
REVOKE ALL ON FUNCTION scheduler_support_channel_groups_from_new() FROM PUBLIC;
REVOKE ALL ON FUNCTION scheduler_support_channel_groups_dirty() FROM PUBLIC;
REVOKE ALL ON FUNCTION scheduler_support_channel_groups_from_old() FROM PUBLIC;
REVOKE ALL ON FUNCTION scheduler_support_dirty_pricing_channels(bigint[]) FROM PUBLIC;
REVOKE ALL ON FUNCTION scheduler_support_channel_pricing_from_new() FROM PUBLIC;
REVOKE ALL ON FUNCTION scheduler_support_channel_pricing_dirty() FROM PUBLIC;
REVOKE ALL ON FUNCTION scheduler_support_channel_pricing_from_old() FROM PUBLIC;
REVOKE ALL ON SEQUENCE scheduler_support_publication_generation_seq FROM PUBLIC;

DROP TRIGGER IF EXISTS scheduler_support_channels_insert_dirty ON channels;
DROP TRIGGER IF EXISTS scheduler_support_channels_update_dirty ON channels;
DROP TRIGGER IF EXISTS scheduler_support_channels_delete_dirty ON channels;
CREATE TRIGGER scheduler_support_channels_insert_dirty
AFTER INSERT ON channels
REFERENCING NEW TABLE AS new_rows
FOR EACH STATEMENT EXECUTE FUNCTION scheduler_support_channels_from_new();
CREATE TRIGGER scheduler_support_channels_update_dirty
AFTER UPDATE ON channels
REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
FOR EACH STATEMENT EXECUTE FUNCTION scheduler_support_channels_dirty();
CREATE TRIGGER scheduler_support_channels_delete_dirty
AFTER DELETE ON channels
REFERENCING OLD TABLE AS old_rows
FOR EACH STATEMENT EXECUTE FUNCTION scheduler_support_channels_from_old();

DROP TRIGGER IF EXISTS scheduler_support_channel_groups_insert_dirty ON channel_groups;
DROP TRIGGER IF EXISTS scheduler_support_channel_groups_update_dirty ON channel_groups;
DROP TRIGGER IF EXISTS scheduler_support_channel_groups_delete_dirty ON channel_groups;
CREATE TRIGGER scheduler_support_channel_groups_insert_dirty
AFTER INSERT ON channel_groups
REFERENCING NEW TABLE AS new_rows
FOR EACH STATEMENT EXECUTE FUNCTION scheduler_support_channel_groups_from_new();
CREATE TRIGGER scheduler_support_channel_groups_update_dirty
AFTER UPDATE ON channel_groups
REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
FOR EACH STATEMENT EXECUTE FUNCTION scheduler_support_channel_groups_dirty();
CREATE TRIGGER scheduler_support_channel_groups_delete_dirty
AFTER DELETE ON channel_groups
REFERENCING OLD TABLE AS old_rows
FOR EACH STATEMENT EXECUTE FUNCTION scheduler_support_channel_groups_from_old();

DROP TRIGGER IF EXISTS scheduler_support_channel_pricing_insert_dirty ON channel_model_pricing;
DROP TRIGGER IF EXISTS scheduler_support_channel_pricing_update_dirty ON channel_model_pricing;
DROP TRIGGER IF EXISTS scheduler_support_channel_pricing_delete_dirty ON channel_model_pricing;
CREATE TRIGGER scheduler_support_channel_pricing_insert_dirty
AFTER INSERT ON channel_model_pricing
REFERENCING NEW TABLE AS new_rows
FOR EACH STATEMENT EXECUTE FUNCTION scheduler_support_channel_pricing_from_new();
CREATE TRIGGER scheduler_support_channel_pricing_update_dirty
AFTER UPDATE ON channel_model_pricing
REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
FOR EACH STATEMENT EXECUTE FUNCTION scheduler_support_channel_pricing_dirty();
CREATE TRIGGER scheduler_support_channel_pricing_delete_dirty
AFTER DELETE ON channel_model_pricing
REFERENCING OLD TABLE AS old_rows
FOR EACH STATEMENT EXECUTE FUNCTION scheduler_support_channel_pricing_from_old();
