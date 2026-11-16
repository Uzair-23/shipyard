package apps

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Uzair-23/shipyard/internal/secrets"
	"github.com/Uzair-23/shipyard/internal/store"
)

// ReservedEnvName reports names users cannot set: the platform owns them.
func ReservedEnvName(name string) bool {
	return name == "PORT" || name == "HOME" || name == "PATH" || strings.HasPrefix(name, "SHIPYARD_")
}

// userEnv returns the app's environment variables as KEY=value pairs,
// decrypted for the container. Values are never logged.
func (p *Pipeline) userEnv(ctx context.Context, appID pgtype.UUID) ([]string, error) {
	rows, err := store.New(p.Pool).ListEnv(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("list env: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	if len(p.MasterKey) != secrets.KeyLen {
		return nil, fmt.Errorf("app %s has environment variables but no master key is configured", appID.String())
	}
	env := make([]string, 0, len(rows))
	for _, row := range rows {
		if ReservedEnvName(row.Key) { // the API refuses these; never let one override PORT
			continue
		}
		plain, err := secrets.Decrypt(p.MasterKey, appID.String(), row.Key, row.ValueEncrypted, row.Nonce)
		if err != nil {
			return nil, fmt.Errorf("decrypt variable %s of app %s: %w", row.Key, appID.String(), err)
		}
		env = append(env, row.Key+"="+string(plain))
	}
	return env, nil
}
