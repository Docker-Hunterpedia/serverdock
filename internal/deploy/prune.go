package deploy

import (
	"context"
	"database/sql"

	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// KeepDeploys is how many deploy rows a service keeps (about 2 KB each).
const KeepDeploys = 200

// PruneDeploys deletes a service's finished deploy rows beyond its newest
// KeepDeploys, never the active deploy and never a queued or running one. The
// runner calls it when a deploy finishes (lazy: no timer). Stages go with their
// deploy (ON DELETE CASCADE). It returns how many rows it deleted.
func PruneDeploys(ctx context.Context, st *store.Store, serviceID int64) (int64, error) {
	var n int64
	err := st.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM deploys WHERE service_id = ?
			AND status IN ('succeeded', 'failed', 'cancelled', 'superseded')
			AND id NOT IN (SELECT id FROM deploys WHERE service_id = ? ORDER BY number DESC LIMIT ?)
			AND id NOT IN (SELECT active_deploy_id FROM services WHERE id = ? AND active_deploy_id IS NOT NULL)`,
			serviceID, serviceID, KeepDeploys, serviceID)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}
