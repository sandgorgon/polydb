// Package hook lets code in this module reach what the public packages
// keep unexported.
package hook

import (
	"context"

	"github.com/sandgorgon/polydb/internal/engine"
)

// Embed returns a polydb.DB running statements on eng as user, using
// database if it isn't "". Package polydb sets it.
var Embed func(ctx context.Context, eng *engine.Engine, user, database string) (any, error)
