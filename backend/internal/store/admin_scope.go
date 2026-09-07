package store

import (
	"context"
	"database/sql"
)

type adminChannelsKey struct{}

// WithAdminChannels constrains console reads and writes; public requests have
// no scope value and continue to resolve public marketing invitations.
func WithAdminChannels(ctx context.Context, channels []string) context.Context {
	return context.WithValue(ctx, adminChannelsKey{}, channels)
}
func adminChannels(ctx context.Context) []string {
	channels, _ := ctx.Value(adminChannelsKey{}).([]string)
	if channels == nil {
		return []string{}
	}
	return channels
}

func adminChannelAllowed(ctx context.Context, channel string) bool {
	channels := adminChannels(ctx)
	if len(channels) == 0 {
		return true
	}
	for _, allowed := range channels {
		if allowed == channel {
			return true
		}
	}
	return false
}

// Re-read dynamic write permission inside the mutation transaction. Channel
// scoped roles cannot use the global user-management endpoints.
func platformUserWriterTx(ctx context.Context, tx *sql.Tx, actorID string) (bool, error) {
	var allowed bool
	err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT a.permissions ? 'users.write' AND jsonb_array_length(a.channels)=0 FROM admin_roles a JOIN users u ON u.admin_role_id=a.id WHERE u.id=$1 FOR SHARE OF a),FALSE)`, actorID).Scan(&allowed)
	return allowed, err
}

func elevatedBaseRole(role string) bool { return role == "admin" || role == "super_admin" }
