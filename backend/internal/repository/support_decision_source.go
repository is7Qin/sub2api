package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type supportDecisionSource struct {
	db      *sql.DB
	beginTx func(context.Context, *sql.TxOptions) (*sql.Tx, error)
}

// NewSupportDecisionSource is wired only into background construction workers;
// request repository interfaces intentionally do not include this capability.
func NewSupportDecisionSource(db *sql.DB) service.SupportDecisionSource {
	return &supportDecisionSource{db: db}
}

func NewSupportDecisionGenerationRepository(db *sql.DB) service.SupportDecisionGenerationRepository {
	return &supportDecisionSource{db: db}
}

func (s *supportDecisionSource) NextSupportDecisionGeneration(ctx context.Context) (uint64, error) {
	var generation int64
	if err := s.db.QueryRowContext(ctx, `SELECT nextval('scheduler_support_publication_generation_seq')`).Scan(&generation); err != nil {
		return 0, fmt.Errorf("allocate support decision generation: %w", err)
	}
	if generation <= 0 {
		return 0, fmt.Errorf("allocate support decision generation: invalid value %d", generation)
	}
	return uint64(generation), nil
}

func (s *supportDecisionSource) Load(ctx context.Context) (*service.SupportDecisionConstructionSnapshot, error) {
	beginTx := s.beginTx
	if beginTx == nil {
		beginTx = s.db.BeginTx
	}
	tx, err := beginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin support decision snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	snapshot := &service.SupportDecisionConstructionSnapshot{}
	if snapshot.Accounts, err = loadSupportDecisionAccounts(ctx, tx); err != nil {
		return nil, fmt.Errorf("load support decision accounts: %w", err)
	}
	if snapshot.Memberships, err = loadSupportDecisionMemberships(ctx, tx); err != nil {
		return nil, fmt.Errorf("load support decision memberships: %w", err)
	}
	if snapshot.Groups, err = loadSupportDecisionGroups(ctx, tx); err != nil {
		return nil, fmt.Errorf("load support decision groups: %w", err)
	}
	if snapshot.Channels, err = loadSupportDecisionChannels(ctx, tx); err != nil {
		return nil, fmt.Errorf("load support decision channels: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit support decision snapshot: %w", err)
	}
	return snapshot, nil
}

func supportDecisionAccountSelectList(alias string) string {
	cols := []string{alias + ".id", alias + ".platform", alias + ".type", alias + ".concurrency"}
	cols = appendProjectedJSONColumns(cols, alias, "credentials", supportDecisionCredentialsSubKeys)
	cols = appendProjectedJSONColumns(cols, alias, "extra", supportDecisionExtraSubKeys)
	return strings.Join(cols, ", ")
}

func loadSupportDecisionAccounts(ctx context.Context, tx *sql.Tx) ([]service.Account, error) {
	query := fmt.Sprintf(`SELECT %s
FROM accounts a
WHERE a.deleted_at IS NULL AND a.status = $1 AND a.schedulable = TRUE
ORDER BY a.id`, supportDecisionAccountSelectList("a"))
	rows, err := tx.QueryContext(ctx, query, service.StatusActive)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	subKeyCount := len(supportDecisionCredentialsSubKeys) + len(supportDecisionExtraSubKeys)
	accounts := make([]service.Account, 0, 16)
	for rows.Next() {
		var account service.Account
		values := make([]sql.NullString, subKeyCount)
		dest := make([]any, 0, 4+subKeyCount)
		dest = append(dest, &account.ID, &account.Platform, &account.Type, &account.Concurrency)
		for i := range values {
			dest = append(dest, &values[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		offset := len(supportDecisionCredentialsSubKeys)
		account.Credentials = decodeProjectedJSONSubKeys(values[:offset], supportDecisionCredentialsSubKeys)
		account.Extra = decodeProjectedJSONSubKeys(values[offset:], supportDecisionExtraSubKeys)
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return accounts, nil
}

func loadSupportDecisionMemberships(ctx context.Context, tx *sql.Tx) ([]service.SupportDecisionMembership, error) {
	rows, err := tx.QueryContext(ctx, `SELECT ag.account_id, ag.group_id
FROM account_groups ag
JOIN accounts a ON a.id = ag.account_id
JOIN groups g ON g.id = ag.group_id
WHERE a.deleted_at IS NULL AND a.status = $1 AND a.schedulable = TRUE
  AND g.deleted_at IS NULL
ORDER BY ag.account_id, ag.group_id`, service.StatusActive)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	memberships := make([]service.SupportDecisionMembership, 0, 16)
	for rows.Next() {
		var membership service.SupportDecisionMembership
		if err := rows.Scan(&membership.AccountID, &membership.GroupID); err != nil {
			return nil, err
		}
		memberships = append(memberships, membership)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return memberships, nil
}

func loadSupportDecisionGroups(ctx context.Context, tx *sql.Tx) ([]service.SupportDecisionGroup, error) {
	rows, err := tx.QueryContext(ctx, `SELECT g.id, g.platform, g.require_privacy_set, g.models_list_config
FROM groups g
WHERE g.deleted_at IS NULL
ORDER BY g.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	groups := make([]service.SupportDecisionGroup, 0, 16)
	for rows.Next() {
		var group service.SupportDecisionGroup
		var modelsJSON []byte
		if err := rows.Scan(&group.ID, &group.Platform, &group.RequirePrivacySet, &modelsJSON); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(modelsJSON, &group.ModelsListConfig); err != nil {
			return nil, fmt.Errorf("decode group %d models_list_config: %w", group.ID, err)
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return groups, nil
}

func loadSupportDecisionChannels(ctx context.Context, tx *sql.Tx) ([]service.SupportDecisionChannel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.id, c.status, c.model_mapping,
       COALESCE(c.restrict_models, FALSE),
       COALESCE(NULLIF(c.billing_model_source, ''), 'channel_mapped'),
       COALESCE((SELECT jsonb_agg(cg.group_id ORDER BY cg.group_id) FROM channel_groups cg WHERE cg.channel_id = c.id), '[]'::jsonb) AS group_ids,
       COALESCE((SELECT jsonb_agg(jsonb_build_object('platform', cmp.platform, 'models', cmp.models) ORDER BY cmp.id) FROM channel_model_pricing cmp WHERE cmp.channel_id = c.id), '[]'::jsonb) AS pricing_models
FROM channels c
ORDER BY c.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	channels := make([]service.SupportDecisionChannel, 0, 8)
	for rows.Next() {
		var channel service.SupportDecisionChannel
		var mappingJSON, groupIDsJSON, pricingJSON []byte
		var restrictModels sql.NullBool
		var billingModelSource sql.NullString
		if err := rows.Scan(&channel.ID, &channel.Status, &mappingJSON, &restrictModels, &billingModelSource, &groupIDsJSON, &pricingJSON); err != nil {
			return nil, err
		}
		channel.RestrictModels = restrictModels.Valid && restrictModels.Bool
		channel.BillingModelSource = billingModelSource.String
		if channel.BillingModelSource == "" {
			channel.BillingModelSource = service.BillingModelSourceChannelMapped
		}
		if len(mappingJSON) > 0 {
			if err := json.Unmarshal(mappingJSON, &channel.ModelMapping); err != nil {
				return nil, fmt.Errorf("decode channel %d model_mapping: %w", channel.ID, err)
			}
		}
		if err := json.Unmarshal(groupIDsJSON, &channel.GroupIDs); err != nil {
			return nil, fmt.Errorf("decode channel %d group_ids: %w", channel.ID, err)
		}
		if err := json.Unmarshal(pricingJSON, &channel.PricingModels); err != nil {
			return nil, fmt.Errorf("decode channel %d pricing models: %w", channel.ID, err)
		}
		channels = append(channels, channel)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return channels, nil
}
