package repository

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/thecd/fundarbitrage/internal/keyrotation"
)

// RotationLogRepo inserts records into key_rotation_log.
// The table has the following CHECK constraints:
//   - action IN ('PLANNED_ROTATION','EMERGENCY_ROTATION','KILL_SWITCH')
//   - action = 'KILL_SWITCH' OR credential_id IS NOT NULL
//
// Record enforces these constraints before hitting the DB so the caller gets
// a clear error rather than a cryptic PostgreSQL violation.
type RotationLogRepo struct {
	pool *pgxpool.Pool
}

// NewRotationLogRepo creates a RotationLogRepo.
func NewRotationLogRepo(pool *pgxpool.Pool) *RotationLogRepo {
	return &RotationLogRepo{pool: pool}
}

// Record inserts one rotation event.
// credentialID may be nil only when action == KillSwitch;
// for all other actions a nil credentialID is rejected with a clear error
// before the query reaches the DB.
func (r *RotationLogRepo) Record(
	ctx context.Context,
	action keyrotation.Action,
	credentialID *int64,
	initiator string,
	details map[string]any,
) error {
	// Validate the constraint that the app level should enforce, but we
	// double-check here to give a clear error message.
	if action != keyrotation.KillSwitch && credentialID == nil {
		return fmt.Errorf("repository: key_rotation_log: credential_id required for action %s (only KILL_SWITCH allows nil)", action)
	}

	var detailsJSON []byte
	if details != nil {
		var err error
		detailsJSON, err = json.Marshal(details)
		if err != nil {
			return fmt.Errorf("repository: key_rotation_log: marshal details: %w", err)
		}
	}

	_, err := r.pool.Exec(ctx, `
		INSERT INTO key_rotation_log (credential_id, action, initiator, details)
		VALUES ($1, $2, $3, $4)
	`, credentialID, string(action), initiator, detailsJSON)
	if err != nil {
		return fmt.Errorf("repository: key_rotation_log: insert %s: %w", action, err)
	}
	return nil
}
