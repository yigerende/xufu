package platform

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
)

const (
	proxyVendorIPRoyal  = "iproyal"
	proxyVendorCliproxy = "cliproxy"
	proxyVendorOther    = "other"
	proxyModeDynamic    = "dynamic"
	proxyModeStatic     = "static"
)

func validProxyVendor(v string) bool {
	return v == proxyVendorIPRoyal || v == proxyVendorCliproxy || v == proxyVendorOther
}

func validProxyMode(v string) bool {
	return v == proxyModeDynamic || v == proxyModeStatic
}

// Only legacy records/clients without an explicit choice use the old host rule.
func legacyProxyVendor(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err == nil && strings.EqualFold(strings.TrimSuffix(u.Hostname(), "."), "geo.iproyal.com") {
		return proxyVendorIPRoyal
	}
	return proxyVendorOther
}

// Backfill the new label without changing credentials or existing order bindings.
func (a *App) migrateProxyVendors(ctx context.Context) error {
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1886003)"); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, "SELECT id,endpoint_enc FROM resources WHERE vendor='' FOR UPDATE")
	if err != nil {
		return err
	}
	legacy, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ ID, Enc string }])
	if err != nil {
		return err
	}
	for _, row := range legacy {
		endpoint, err := a.decrypt(row.Enc)
		if err != nil {
			return errors.New("proxy vendor migration: credentials unavailable")
		}
		if _, err = tx.Exec(ctx, "UPDATE resources SET vendor=$2 WHERE id=$1 AND vendor=''", row.ID, legacyProxyVendor(endpoint)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
