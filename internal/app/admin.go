package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"fridge/internal/engine"
	"fridge/internal/store"
	"fridge/internal/swarm"
)

// AdminConn runs SQL as a node's superuser. dsn already carries the credentials; nothing from it may be logged.
type AdminConn interface {
	Exec(ctx context.Context, dsn, sql string) error
}

// pgxAdmin connects over TCP (the node's edge sidecar), retrying until the database accepts connections or ctx ends:
// a node that has just been scheduled is not listening yet.
type pgxAdmin struct{}

func (pgxAdmin) Exec(ctx context.Context, dsn, sql string) error {
	var last error
	for {
		err := pgxExecOnce(ctx, dsn, sql)
		if err == nil {
			return nil
		}
		var pe interface{ SQLState() string }
		if errors.As(err, &pe) { // the server answered and refused the statement: retrying cannot help
			return err
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last error: %v)", ctx.Err(), last)
		case <-time.After(time.Second):
		}
	}
}

func pgxExecOnce(ctx context.Context, dsn, sql string) error {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := pgx.Connect(cctx, dsn)
	if err != nil {
		return err
	}
	defer c.Close(context.WithoutCancel(ctx))
	_, err = c.Exec(cctx, sql, pgx.QueryExecModeSimpleProtocol) // several statements, no parameters
	return err
}

func (a *App) admin() AdminConn {
	if a.Admin != nil {
		return a.Admin
	}
	return pgxAdmin{}
}

// execAsAdmin runs sql on node d as its superuser (password adminPW), through the node's edge sidecar.
func (a *App) execAsAdmin(ctx context.Context, d store.Database, eng engine.Engine, adminPW, sql string) error {
	p := nodeParams(d)
	p.AdminPassword = adminPW
	dsn := eng.AdminDSN(p, swarm.EdgeService(d.NodeKey), swarm.EdgePort)
	return a.admin().Exec(ctx, dsn, sql)
}
