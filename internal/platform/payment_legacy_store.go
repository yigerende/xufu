package platform

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

func (a *App) legacyPaymentInput(ctx context.Context, id, attempt string, in *paymentInput) error {
	var sealed string
	if err := a.DB.QueryRow(ctx, "SELECT legacy_checkpoint_enc,executor FROM payment_runs WHERE order_id=$1 AND attempt_id=$2", id, attempt).Scan(&sealed, &in.Executor); err != nil {
		return err
	}
	if in.Mode == "check" && sealed != "" {
		raw, err := a.decrypt(sealed)
		if err != nil || json.Unmarshal([]byte(raw), &in.Checkpoint) != nil {
			return errors.New("invalid_response")
		}
		// Older protocol orders predate the executor column. Their original
		// encrypted checkpoint remains authoritative for GET-only recovery.
		in.Executor = paymentExecutorLegacyHTTP
	}
	var auth string
	if err := a.DB.QueryRow(ctx, "SELECT credentials_enc FROM subscription_runs WHERE order_id=$1 AND credentials_expires_at>now()", id).Scan(&auth); err == nil && auth != "" {
		raw, err := a.decrypt(auth)
		if err != nil || json.Unmarshal([]byte(raw), &in.Authorization) != nil {
			return errors.New("invalid_session")
		}
		in.SessionCookies = browserSessionCookies(in.Authorization)
	}
	in.SaveCheckpoint = func(ctx context.Context, cp legacyPaymentCheckpoint) error {
		raw, err := json.Marshal(cp)
		if err != nil {
			return err
		}
		enc, err := a.encrypt(string(raw))
		if err != nil {
			return err
		}
		tag, err := a.DB.Exec(ctx, `UPDATE payment_runs SET legacy_checkpoint_enc=$3 WHERE order_id=$1 AND attempt_id=$2 AND state IN ('preparing','quoted','submitting','checking') AND expires_at>now()`, id, attempt, enc)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrLeaseLost
		}
		return nil
	}
	return nil
}

func (a *App) legacyPaymentGuard(id, attempt, lease string) func(context.Context) error {
	return func(ctx context.Context) error {
		var expires time.Time
		err := a.DB.QueryRow(ctx, `SELECT s.expires_at FROM payment_runs p
 JOIN orders o ON o.id=p.order_id JOIN order_proxy_sessions s ON s.order_id=p.order_id
 JOIN resources r ON r.id=s.resource_id JOIN proxy_leases l ON l.resource_id=s.resource_id AND l.id=$3 AND l.expires_at>now()
 WHERE p.order_id=$1 AND p.attempt_id=$2 AND p.state IN ('preparing','quoted','submitting','checking') AND p.expires_at>now()
 AND o.status IN ('processing','review') AND r.enabled AND s.endpoint_enc<>''
 AND NOT EXISTS(SELECT 1 FROM subscription_protocol c WHERE c.order_id=p.order_id)`, id, attempt, lease).Scan(&expires)
		if err != nil {
			return errors.New("proxy_session_unavailable")
		}
		if !expires.After(time.Now()) {
			return errors.New("proxy_session_expired")
		}
		return nil
	}
}
