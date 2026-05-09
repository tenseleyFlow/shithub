// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import "github.com/jackc/pgx/v5/pgconn"

// pgconnError aliases pgconn.PgError so create.go's isUniqueViolation
// helper can errors.As to a private name without leaking the import
// to every file in the package.
type pgconnError = pgconn.PgError
