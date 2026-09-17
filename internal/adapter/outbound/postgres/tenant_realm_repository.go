package postgres

import (
	"context"
	"errors"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const tenantRealmColumns = `tenant_id, realm_name, strategy, keycloak_shard, status,
	realm_template_version, trial_ends_at, converted_at, disabled_at, suspension_source, exported_at,
	export_s3_key, service_account_principal_id, service_account_keycloak_client_id,
	record_version, created_at, updated_at, deleted_at`

// TenantRealmRepository implements port.TenantRealmRepository (§4.2).
type TenantRealmRepository struct {
	pool *pgcommon.Pool
}

// NewTenantRealmRepository constructs a TenantRealmRepository backed by
// pool.
func NewTenantRealmRepository(pool *pgcommon.Pool) *TenantRealmRepository {
	return &TenantRealmRepository{pool: pool}
}

func scanTenantRealm(row pgx.Row) (*domain.TenantRealm, error) {
	var t domain.TenantRealm
	err := row.Scan(&t.TenantID, &t.RealmName, &t.Strategy, &t.KeycloakShard, &t.Status,
		&t.RealmTemplateVersion, &t.TrialEndsAt, &t.ConvertedAt, &t.DisabledAt, &t.SuspensionSource, &t.ExportedAt,
		&t.ExportS3Key, &t.ServiceAccountPrincipalID, &t.ServiceAccountKeycloakClientID,
		&t.RecordVersion, &t.CreatedAt, &t.UpdatedAt, &t.DeletedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// FindByTenantID returns tenantID's non-deleted realm row, or
// ErrTenantRealmNotFound.
func (r *TenantRealmRepository) FindByTenantID(ctx context.Context, tenantID uuid.UUID) (*domain.TenantRealm, error) {
	var out *domain.TenantRealm
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+tenantRealmColumns+` FROM tenant_realms WHERE tenant_id = $1 AND deleted_at IS NULL`, tenantID)
		found, scanErr := scanTenantRealm(row)
		if scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				return domain.NewError(domain.ErrTenantRealmNotFound, "no tenant_realms row for this tenant")
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

// Upsert inserts a new tenant_realms row, or updates the mutable columns of
// an existing one keyed by tenant_id (RP-1/RP-2 first-touch semantics, §4.2).
func (r *TenantRealmRepository) Upsert(ctx context.Context, t *domain.TenantRealm) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO tenant_realms (tenant_id, realm_name, strategy, keycloak_shard, status,
				realm_template_version, trial_ends_at, converted_at, disabled_at, suspension_source, exported_at, export_s3_key,
				service_account_principal_id, service_account_keycloak_client_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			ON CONFLICT (tenant_id) DO UPDATE SET
				realm_name = EXCLUDED.realm_name,
				strategy = EXCLUDED.strategy,
				keycloak_shard = EXCLUDED.keycloak_shard,
				status = EXCLUDED.status,
				realm_template_version = EXCLUDED.realm_template_version,
				trial_ends_at = EXCLUDED.trial_ends_at,
				converted_at = EXCLUDED.converted_at,
				disabled_at = EXCLUDED.disabled_at,
				suspension_source = EXCLUDED.suspension_source,
				exported_at = EXCLUDED.exported_at,
				export_s3_key = EXCLUDED.export_s3_key,
				service_account_principal_id = EXCLUDED.service_account_principal_id,
				service_account_keycloak_client_id = EXCLUDED.service_account_keycloak_client_id,
				record_version = tenant_realms.record_version + 1,
				updated_at = now()`,
			t.TenantID, t.RealmName, t.Strategy, t.KeycloakShard, t.Status,
			t.RealmTemplateVersion, t.TrialEndsAt, t.ConvertedAt, t.DisabledAt, t.SuspensionSource, t.ExportedAt, t.ExportS3Key,
			t.ServiceAccountPrincipalID, t.ServiceAccountKeycloakClientID)
		return err
	})
}

// Update performs an optimistic-locked UPDATE keyed on tenant_id +
// record_version (§9.6) — 0 rows affected means a concurrent writer won,
// surfaced as ErrOptimisticLockConflict.
func (r *TenantRealmRepository) Update(ctx context.Context, t *domain.TenantRealm) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE tenant_realms SET
				realm_name = $2, strategy = $3, keycloak_shard = $4, status = $5,
				realm_template_version = $6, trial_ends_at = $7, converted_at = $8,
				disabled_at = $9, suspension_source = $10, exported_at = $11, export_s3_key = $12,
				service_account_principal_id = $13, service_account_keycloak_client_id = $14,
				record_version = record_version + 1, updated_at = now()
			WHERE tenant_id = $1 AND record_version = $15`,
			t.TenantID, t.RealmName, t.Strategy, t.KeycloakShard, t.Status,
			t.RealmTemplateVersion, t.TrialEndsAt, t.ConvertedAt, t.DisabledAt, t.SuspensionSource, t.ExportedAt,
			t.ExportS3Key, t.ServiceAccountPrincipalID, t.ServiceAccountKeycloakClientID, t.RecordVersion)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.NewError(domain.ErrOptimisticLockConflict, "tenant_realms record_version conflict").
				WithDetails(map[string]any{"record_version": t.RecordVersion})
		}
		return nil
	})
}

// CountsByShard returns the current live dedicated-realm count per
// keycloak_shard (§9.5). Runs against a BYPASSRLS pool since placement is
// cross-tenant by nature.
func (r *TenantRealmRepository) CountsByShard(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT keycloak_shard, count(*) FROM tenant_realms
			WHERE strategy = 'dedicated' AND deleted_at IS NULL AND keycloak_shard IS NOT NULL
			GROUP BY keycloak_shard`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var shard string
			var n int
			if err := rows.Scan(&shard, &n); err != nil {
				return err
			}
			out[shard] = n
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListExpiredTrials returns shared-trial realms past their trial_ends_at
// (RP-C1, idx_tenant_realms_trial_exp). Runs against a BYPASSRLS pool
// (sysPool) since the reconciler sweeps across every tenant.
func (r *TenantRealmRepository) ListExpiredTrials(ctx context.Context, limit int) ([]*domain.TenantRealm, error) {
	var out []*domain.TenantRealm
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+tenantRealmColumns+` FROM tenant_realms
			WHERE strategy = 'shared' AND status = 'active' AND trial_ends_at < now()
			ORDER BY trial_ends_at ASC
			LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanTenantRealm(rows)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListSuspendedPastRetention returns dedicated realms suspended by the
// RP-C3 billing-lapse sweep (never operator suspensions, §8.8 —
// suspension_source narrows this at the SQL level, not in application
// code) whose disabled_at is older than retentionDays
// (idx_tenant_realms_lapse_offboard). Runs against a BYPASSRLS pool since
// the reconciler sweeps across every tenant.
func (r *TenantRealmRepository) ListSuspendedPastRetention(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
	var out []*domain.TenantRealm
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+tenantRealmColumns+` FROM tenant_realms
			WHERE status = 'suspended' AND suspension_source = 'billing_lapse'
				AND disabled_at < now() - make_interval(days => $1)
			ORDER BY disabled_at ASC
			LIMIT $2`, retentionDays, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanTenantRealm(rows)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
