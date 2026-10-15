package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/google/uuid"
)

const idpConfigColumns = `id, tenant_id, idp_alias, protocol, display_name, enabled, config, status, record_version, created_at, updated_at`

// IdpConfigRepository implements port.IdpConfigRepository (§4.3).
type IdpConfigRepository struct {
	pool *pgcommon.Pool
}

// NewIdpConfigRepository constructs an IdpConfigRepository backed by pool.
func NewIdpConfigRepository(pool *pgcommon.Pool) *IdpConfigRepository {
	return &IdpConfigRepository{pool: pool}
}

func scanIdpConfig(row pgcommon.Row) (*domain.IdpConfig, error) {
	var c domain.IdpConfig
	var cfg []byte
	if err := row.Scan(&c.ID, &c.TenantID, &c.IdpAlias, &c.Protocol, &c.DisplayName, &c.Enabled, &cfg, &c.Status, &c.RecordVersion, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	if len(cfg) > 0 {
		_ = json.Unmarshal(cfg, &c.Config) //nolint:errcheck // best-effort decode of this service's own prior JSON write; a failure leaves Config zero-valued rather than failing the whole read
	}
	return &c, nil
}

// FindByAlias returns tenantID's IdP config for alias, or ErrIdpConfigNotFound.
func (r *IdpConfigRepository) FindByAlias(ctx context.Context, tenantID uuid.UUID, alias string) (*domain.IdpConfig, error) {
	var out *domain.IdpConfig
	err := withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+idpConfigColumns+` FROM keycloak_idp_configs WHERE tenant_id = $1 AND idp_alias = $2`, tenantID, alias)
		found, scanErr := scanIdpConfig(row)
		if scanErr != nil {
			if errors.Is(scanErr, pgcommon.ErrNoRows) {
				return domain.NewError(domain.ErrIdpConfigNotFound, "no such idp config")
			}
			return scanErr
		}
		out = found
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CountActiveByTenant returns the count of IdP configurations for tenantID
// with status = 'active', used by RP-11 (CD-20 SSO-downgrade guard).
// Disabled and pending_validation configs are excluded from the count.
func (r *IdpConfigRepository) CountActiveByTenant(ctx context.Context, tenantID uuid.UUID) (int, error) {
	var n int
	err := withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM keycloak_idp_configs WHERE tenant_id = $1 AND status = 'active'`,
			tenantID,
		).Scan(&n)
	})
	return n, err
}

// ListByTenant returns every IdP config for tenantID, ordered by alias.
func (r *IdpConfigRepository) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*domain.IdpConfig, error) {
	var out []*domain.IdpConfig
	err := withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+idpConfigColumns+` FROM keycloak_idp_configs WHERE tenant_id = $1 ORDER BY idp_alias`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanIdpConfig(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListAll returns every IdP config across every tenant (RP-13). Callers
// must pass a BYPASSRLS pool (sysPool) since this is deliberately
// cross-tenant.
func (r *IdpConfigRepository) ListAll(ctx context.Context) ([]*domain.IdpConfig, error) {
	var out []*domain.IdpConfig
	err := withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+idpConfigColumns+` FROM keycloak_idp_configs ORDER BY tenant_id, idp_alias`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanIdpConfig(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Upsert inserts c or, on a (tenant_id, idp_alias) conflict, updates it and
// bumps record_version.
func (r *IdpConfigRepository) Upsert(ctx context.Context, c *domain.IdpConfig) error {
	cfg, err := json.Marshal(c.Config)
	if err != nil {
		return err
	}
	return withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO keycloak_idp_configs (id, tenant_id, idp_alias, protocol, display_name, enabled, config, status)
			VALUES (COALESCE($1, gen_random_uuid()), $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (tenant_id, idp_alias) DO UPDATE SET
				protocol = EXCLUDED.protocol,
				display_name = EXCLUDED.display_name,
				enabled = EXCLUDED.enabled,
				config = EXCLUDED.config,
				status = EXCLUDED.status,
				record_version = keycloak_idp_configs.record_version + 1,
				updated_at = now()`,
			nilIfZeroUUID(c.ID), c.TenantID, c.IdpAlias, c.Protocol, c.DisplayName, c.Enabled, cfg, c.Status)
		return err
	})
}

// Delete removes tenantID's IdP config for alias.
func (r *IdpConfigRepository) Delete(ctx context.Context, tenantID uuid.UUID, alias string) error {
	return withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM keycloak_idp_configs WHERE tenant_id = $1 AND idp_alias = $2`, tenantID, alias)
		return err
	})
}

func nilIfZeroUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// IdpValidationRepository implements port.IdpValidationRepository (§4.3) —
// append-only.
type IdpValidationRepository struct {
	pool *pgcommon.Pool
}

// NewIdpValidationRepository constructs an IdpValidationRepository backed
// by pool.
func NewIdpValidationRepository(pool *pgcommon.Pool) *IdpValidationRepository {
	return &IdpValidationRepository{pool: pool}
}

// Insert appends v to the append-only validation history.
func (r *IdpValidationRepository) Insert(ctx context.Context, v *domain.IdpConfigValidation) error {
	checks, _ := json.Marshal(v.Checks)         //nolint:errcheck // map[string]string can never fail to marshal
	acks, _ := json.Marshal(v.Acknowledgements) //nolint:errcheck // domain.IdpValidationAck's fields (string/uuid.UUID/time.Time, always set via time.Now()) can never fail to marshal
	return withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO keycloak_idp_config_validations (tenant_id, idp_alias, validator_version, outcome, checks, acknowledgements)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			v.TenantID, v.IdpAlias, v.ValidatorVersion, v.Outcome, checks, acks)
		return err
	})
}

// LatestForAlias returns the most recent validation record for tenantID's
// alias, or nil if none exists.
func (r *IdpValidationRepository) LatestForAlias(ctx context.Context, tenantID uuid.UUID, alias string) (*domain.IdpConfigValidation, error) {
	var out *domain.IdpConfigValidation
	err := withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT id, tenant_id, idp_alias, validator_version, outcome, checks, acknowledgements, validated_at
			FROM keycloak_idp_config_validations
			WHERE tenant_id = $1 AND idp_alias = $2
			ORDER BY validated_at DESC LIMIT 1`, tenantID, alias)
		var v domain.IdpConfigValidation
		var checks, acks []byte
		if err := row.Scan(&v.ID, &v.TenantID, &v.IdpAlias, &v.ValidatorVersion, &v.Outcome, &checks, &acks, &v.ValidatedAt); err != nil {
			if errors.Is(err, pgcommon.ErrNoRows) {
				return nil
			}
			return err
		}
		_ = json.Unmarshal(checks, &v.Checks)         //nolint:errcheck // best-effort decode of this service's own prior JSON write
		_ = json.Unmarshal(acks, &v.Acknowledgements) //nolint:errcheck // best-effort decode of this service's own prior JSON write
		out = &v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
