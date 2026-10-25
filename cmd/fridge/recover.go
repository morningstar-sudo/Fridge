package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"fridge/internal/app"
	"fridge/internal/audit"
	"fridge/internal/auth"
	"fridge/internal/store"
)

// recover is the way back in when nobody can log in: it works on the metadata database directly, so it runs
// inside the server container (docker compose exec fridge /fridge recover …) with the server's environment.
func (c *cli) recover(args []string) error {
	if len(args) == 0 {
		return usageErr("recover: missing subcommand (allowlist | reset-password | promote)")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "allowlist", "reset-password", "promote":
	default:
		return usageErr("recover: unknown subcommand " + sub)
	}
	n := 1
	if sub == "allowlist" {
		n = -1 // one or more CIDRs
	}
	pos, err := c.need("recover "+sub, rest, n, nil)
	if err != nil {
		return err
	}
	dburl := os.Getenv("FRIDGE_DATABASE_URL")
	if dburl == "" {
		return errors.New("recover needs the server's environment (FRIDGE_DATABASE_URL): run it inside the server container")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dburl)
	if err != nil {
		return fmt.Errorf("metadata db: %w", err)
	}
	defer st.Close()
	log := &audit.Logger{Store: st}
	record := func(action, typ, id string, err error, detail map[string]any) {
		log.Record(ctx, store.AuditEntry{Actor: "recover", Action: action, TargetType: typ, TargetID: id, Success: err == nil, Detail: detail})
	}

	switch sub {
	case "allowlist":
		dir := os.Getenv("FRIDGE_TRAEFIK_DIR")
		if dir == "" {
			dir = "/traefik"
		}
		if dir, err = filepath.Abs(dir); err != nil {
			return err
		}
		backend := os.Getenv("FRIDGE_API_BACKEND")
		if backend == "" {
			backend = "http://fridge:8080"
		}
		a := &app.App{Store: st, TraefikDir: dir, APIBackend: backend,
			WebCert: filepath.Join(dir, "certs", "web.crt"), WebKey: filepath.Join(dir, "certs", "web.key")}
		err = a.SetAllowlist(ctx, pos)
		record("recover.allowlist", "setting", "allowlist", err, map[string]any{"cidrs": pos})
		if err != nil {
			return err
		}
		fmt.Fprintln(c.out, "allowlist set:", strings.Join(pos, " "))
		return nil
	}

	key, err := base64.StdEncoding.DecodeString(os.Getenv("FRIDGE_SECRET_KEY"))
	if err != nil || len(key) != 32 {
		return errors.New("recover needs the server's environment (FRIDGE_SECRET_KEY): run it inside the server container")
	}
	au, err := auth.New(st, key, "")
	if err != nil {
		return err
	}
	u, err := st.GetUserByName(ctx, pos[0])
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no account named %q", pos[0])
	} else if err != nil {
		return err
	}
	id := fmt.Sprint(u.ID)
	switch sub {
	case "promote":
		err = au.Promote(ctx, u.Username)
		record("recover.promote", "user", id, err, nil)
		if err != nil {
			return err
		}
		fmt.Fprintf(c.out, "%s is an admin\n", u.Username)
	case "reset-password":
		var tok string
		if tok, err = au.CreateResetInvite(ctx, nil, u.ID); err == nil {
			host := "localhost"
			if h := strings.TrimSpace(strings.Split(os.Getenv("FRIDGE_PUBLIC_HOSTS"), ",")[0]); h != "" {
				host = h
			}
			fmt.Fprintf(c.out, "https://%s:8443/#/invite/%s\n", host, tok)
		}
		record("recover.reset_password", "user", id, err, nil)
		if err != nil {
			return err
		}
		fmt.Fprintln(c.errw, "Open the link in a browser within 48 hours and choose a new password; it works once.")
	}
	return nil
}
