// Package bootstrap embeds the superuser-only bootstrap SQL (plan 6.1). It
// contains no logic; internal/bootstrap executes it.
package bootstrap

import _ "embed"

// RolesSQL creates and configures the cluster roles.
//
//go:embed roles.sql
var RolesSQL string
