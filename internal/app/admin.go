package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"fridge/internal/engine"
	"fridge/internal/job"
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
	if eng.AdminDSN(engine.Params{}, "h", 1) == "" { // no Go driver for this engine: an admin job runs the script
		return a.execViaJob(ctx, d, eng, adminPW, sql)
	}
	p := nodeParams(d)
	p.AdminPassword = adminPW
	dsn := eng.AdminDSN(p, swarm.EdgeService(d.NodeKey), swarm.EdgePort)
	return a.admin().Exec(ctx, dsn, sql)
}

// execViaJob runs script as the node's superuser in a job on the database network, with the node's client
// certificate. The error never carries the script (it holds passwords) nor the tool's output (it may echo it).
func (a *App) execViaJob(ctx context.Context, d store.Database, eng engine.Engine, adminPW, script string) error {
	p := nodeParams(d)
	p.AdminPassword = adminPW
	p.Host = swarm.DBService(d.NodeKey)
	p.Port = eng.Spec(p).Port
	t, err := a.nodeTLS(ctx, d)
	if err != nil {
		return err
	}
	p.TLS = t
	_, stderr, err := a.runJob(ctx, "exec", job.Spec{Op: "exec", Engine: d.Engine, Params: p, Script: script}, []string{a.DBNetwork}, 5*time.Minute)
	if err != nil {
		log.Printf("admin job on %s: %v: %s", d.Name, err, redact(strings.TrimSpace(stderr), script))
		return fmt.Errorf("admin job on %s failed (see the server log)", d.Name)
	}
	return nil
}

// redact blanks every word of 8+ characters of script (passwords) that a tool echoed in its error output.
func redact(out, script string) string {
	for _, w := range strings.FieldsFunc(script, func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || r == '-')
	}) {
		if len(w) >= 8 {
			out = strings.ReplaceAll(out, w, "***")
		}
	}
	return out
}
