DO $$
DECLARE
    usage_logs_oid OID := to_regclass('public.usage_logs');
    accounts_oid OID := to_regclass('public.accounts');
    relation_row RECORD;
    constraint_row RECORD;
    rebuild_account_fk BOOLEAN;
BEGIN
    IF usage_logs_oid IS NULL OR accounts_oid IS NULL THEN
        RAISE EXCEPTION 'usage_logs and accounts must exist before migration 169';
    END IF;

    -- DROP NOT NULL recurses from a partitioned parent; visiting descendants
    -- explicitly also repairs layouts whose child metadata was changed manually.
    FOR relation_row IN
        WITH RECURSIVE usage_relations AS (
            SELECT usage_logs_oid AS relid, 0 AS depth
            UNION ALL
            SELECT inh.inhrelid, parent.depth + 1
            FROM pg_inherits AS inh
            JOIN usage_relations AS parent ON parent.relid = inh.inhparent
        )
        SELECT
            rel.oid,
            ns.nspname,
            rel.relname,
            tree.depth
        FROM usage_relations AS tree
        JOIN pg_class AS rel ON rel.oid = tree.relid
        JOIN pg_namespace AS ns ON ns.oid = rel.relnamespace
        WHERE rel.relkind IN ('r', 'p')
        ORDER BY tree.depth, rel.oid
    LOOP
        EXECUTE format(
            'ALTER TABLE %I.%I ALTER COLUMN account_id DROP NOT NULL',
            relation_row.nspname,
            relation_row.relname
        );
    END LOOP;

    SELECT
        NOT (
            COUNT(*) = 1
            AND COALESCE(BOOL_AND(
                c.conname = 'usage_logs_account_id_fkey'
                AND c.confdeltype = 'n'
                AND c.convalidated
            ), FALSE)
        )
    INTO rebuild_account_fk
    FROM pg_constraint AS c
    JOIN pg_attribute AS src
      ON src.attrelid = c.conrelid
     AND src.attname = 'account_id'
    JOIN pg_attribute AS ref
      ON ref.attrelid = accounts_oid
     AND ref.attname = 'id'
    WHERE c.conrelid = usage_logs_oid
      AND c.contype = 'f'
      AND c.confrelid = accounts_oid
      AND c.conkey = ARRAY[src.attnum]::smallint[]
      AND c.confkey = ARRAY[ref.attnum]::smallint[];

    -- Partition descendants may carry legacy standalone FKs in addition to
    -- parent-generated clones. Drop only exact standalone account FKs; never
    -- drop inherited clones directly. This also prevents name collisions if the
    -- parent relationship is rebuilt and clones are recreated.
    FOR relation_row IN
        WITH RECURSIVE usage_relations AS (
            SELECT usage_logs_oid AS relid, 0 AS depth
            UNION ALL
            SELECT inh.inhrelid, parent.depth + 1
            FROM pg_inherits AS inh
            JOIN usage_relations AS parent ON parent.relid = inh.inhparent
        )
        SELECT rel.oid, ns.nspname, rel.relname, tree.depth
        FROM usage_relations AS tree
        JOIN pg_class AS rel ON rel.oid = tree.relid
        JOIN pg_namespace AS ns ON ns.oid = rel.relnamespace
        WHERE tree.relid <> usage_logs_oid
          AND rel.relkind IN ('r', 'p')
          AND rel.relispartition
        ORDER BY tree.depth DESC, rel.oid
    LOOP
        FOR constraint_row IN
            SELECT c.conname
            FROM pg_constraint AS c
            JOIN pg_attribute AS src
              ON src.attrelid = c.conrelid
             AND src.attname = 'account_id'
            JOIN pg_attribute AS ref
              ON ref.attrelid = accounts_oid
             AND ref.attname = 'id'
            WHERE c.conrelid = relation_row.oid
              AND c.contype = 'f'
              AND c.confrelid = accounts_oid
              AND c.conkey = ARRAY[src.attnum]::smallint[]
              AND c.confkey = ARRAY[ref.attnum]::smallint[]
              AND c.conparentid = 0
        LOOP
            EXECUTE format(
                'ALTER TABLE %I.%I DROP CONSTRAINT %I',
                relation_row.nspname,
                relation_row.relname,
                constraint_row.conname
            );
        END LOOP;
    END LOOP;

    -- Ordinary INHERITS descendants do not inherit foreign keys. Repair each
    -- independently even when the root relationship is already canonical.
    FOR relation_row IN
        WITH RECURSIVE usage_relations AS (
            SELECT usage_logs_oid AS relid, 0 AS depth
            UNION ALL
            SELECT inh.inhrelid, parent.depth + 1
            FROM pg_inherits AS inh
            JOIN usage_relations AS parent ON parent.relid = inh.inhparent
        )
        SELECT rel.oid, ns.nspname, rel.relname, tree.depth
        FROM usage_relations AS tree
        JOIN pg_class AS rel ON rel.oid = tree.relid
        JOIN pg_namespace AS ns ON ns.oid = rel.relnamespace
        WHERE tree.relid <> usage_logs_oid
          AND rel.relkind IN ('r', 'p')
          AND NOT rel.relispartition
        ORDER BY tree.depth, rel.oid
    LOOP
        IF NOT (
            SELECT
                COUNT(*) = 1
                AND COALESCE(BOOL_AND(
                    c.conname = 'usage_logs_account_id_fkey'
                    AND c.confdeltype = 'n'
                    AND c.convalidated
                    AND c.conparentid = 0
                ), FALSE)
            FROM pg_constraint AS c
            JOIN pg_attribute AS src
              ON src.attrelid = c.conrelid
             AND src.attname = 'account_id'
            JOIN pg_attribute AS ref
              ON ref.attrelid = accounts_oid
             AND ref.attname = 'id'
            WHERE c.conrelid = relation_row.oid
              AND c.contype = 'f'
              AND c.confrelid = accounts_oid
              AND c.conkey = ARRAY[src.attnum]::smallint[]
              AND c.confkey = ARRAY[ref.attnum]::smallint[]
        ) THEN
            FOR constraint_row IN
                SELECT c.conname
                FROM pg_constraint AS c
                JOIN pg_attribute AS src
                  ON src.attrelid = c.conrelid
                 AND src.attname = 'account_id'
                JOIN pg_attribute AS ref
                  ON ref.attrelid = accounts_oid
                 AND ref.attname = 'id'
                WHERE c.conrelid = relation_row.oid
                  AND c.contype = 'f'
                  AND c.confrelid = accounts_oid
                  AND c.conkey = ARRAY[src.attnum]::smallint[]
                  AND c.confkey = ARRAY[ref.attnum]::smallint[]
                  AND c.conparentid = 0
            LOOP
                EXECUTE format(
                    'ALTER TABLE %I.%I DROP CONSTRAINT %I',
                    relation_row.nspname,
                    relation_row.relname,
                    constraint_row.conname
                );
            END LOOP;

            EXECUTE format(
                'ALTER TABLE %I.%I ADD CONSTRAINT %I FOREIGN KEY (account_id) REFERENCES public.accounts(id) ON DELETE SET NULL NOT VALID',
                relation_row.nspname,
                relation_row.relname,
                'usage_logs_account_id_fkey'
            );
            EXECUTE format(
                'ALTER TABLE %I.%I VALIDATE CONSTRAINT %I',
                relation_row.nspname,
                relation_row.relname,
                'usage_logs_account_id_fkey'
            );
        END IF;
    END LOOP;

    IF rebuild_account_fk THEN
        -- Drop only the root's exact account FK; PostgreSQL removes partition
        -- clones with it, while ordinary descendants were repaired above.
        FOR relation_row IN
            SELECT rel.oid, ns.nspname, rel.relname
            FROM pg_class AS rel
            JOIN pg_namespace AS ns ON ns.oid = rel.relnamespace
            WHERE rel.oid = usage_logs_oid
        LOOP
            FOR constraint_row IN
                SELECT c.conname
                FROM pg_constraint AS c
                JOIN pg_attribute AS src
                  ON src.attrelid = c.conrelid
                 AND src.attname = 'account_id'
                JOIN pg_attribute AS ref
                  ON ref.attrelid = accounts_oid
                 AND ref.attname = 'id'
                WHERE c.conrelid = relation_row.oid
                  AND c.contype = 'f'
                  AND c.confrelid = accounts_oid
                  AND c.conkey = ARRAY[src.attnum]::smallint[]
                  AND c.confkey = ARRAY[ref.attnum]::smallint[]
                  AND c.conparentid = 0
            LOOP
                EXECUTE format(
                    'ALTER TABLE %I.%I DROP CONSTRAINT %I',
                    relation_row.nspname,
                    relation_row.relname,
                    constraint_row.conname
                );
            END LOOP;
        END LOOP;

        IF (SELECT relkind FROM pg_class WHERE oid = usage_logs_oid) = 'p' THEN
            -- PostgreSQL does not support NOT VALID foreign keys on partitioned
            -- parents, so this validated add is the supported partition path.
            ALTER TABLE public.usage_logs
                ADD CONSTRAINT usage_logs_account_id_fkey
                FOREIGN KEY (account_id)
                REFERENCES public.accounts(id)
                ON DELETE SET NULL;
        ELSE
            -- NOT VALID avoids the ADD-time table scan. Because validation is in
            -- this transaction, the stronger ADD lock is still held until commit.
            ALTER TABLE public.usage_logs
                ADD CONSTRAINT usage_logs_account_id_fkey
                FOREIGN KEY (account_id)
                REFERENCES public.accounts(id)
                ON DELETE SET NULL
                NOT VALID;
            ALTER TABLE public.usage_logs
                VALIDATE CONSTRAINT usage_logs_account_id_fkey;
        END IF;
    END IF;
END $$;
