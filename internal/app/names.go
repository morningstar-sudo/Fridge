package app

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"fridge/internal/store"
)

var (
	dbSuffixRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,29}$`)
	userSuffixRe = regexp.MustCompile(`^[a-z0-9_]{1,15}$`)
)

// dbName returns "<owner>_<suffix>"; a suffix already carrying the owner's own prefix is accepted.
func dbName(owner, suffix string) (string, error) {
	suffix = strings.TrimPrefix(suffix, owner+"_")
	if !dbSuffixRe.MatchString(suffix) {
		return "", invalid("name suffix must match %s", dbSuffixRe)
	}
	return owner + "_" + suffix, nil
}

// dbUser returns "<owner>_<suffix>"; empty suffix derives from the DB name suffix.
func dbUser(owner, suffix, dbSuffix string) (string, error) {
	if suffix == "" {
		suffix = strings.ReplaceAll(dbSuffix, "-", "_")
		if len(suffix) > 15 {
			suffix = suffix[:15]
		}
	}
	suffix = strings.TrimPrefix(suffix, owner+"_")
	if !userSuffixRe.MatchString(suffix) {
		return "", invalid("db_user suffix must match %s", userSuffixRe)
	}
	return owner + "_" + suffix, nil
}

func validDBPassword(pw string) error {
	if len(pw) < 12 || len(pw) > 128 {
		return invalid("db_password must be 12-128 characters")
	}
	for i := 0; i < len(pw); i++ {
		if pw[i] < 0x21 || pw[i] > 0x7e {
			return invalid("db_password must be printable ASCII without spaces")
		}
	}
	return nil
}

// resolveNames validates name, app user and domain for the owner and returns the real name, user and domain id.
func (a *App) resolveNames(ctx context.Context, req CreateRequest) (name, user string, domainID int64, err error) {
	owner, err := a.Store.GetUser(ctx, req.OwnerID)
	if err != nil {
		return "", "", 0, err
	}
	if name, err = dbName(owner.Username, req.Name); err != nil {
		return
	}
	if user, err = dbUser(owner.Username, req.DBUser, strings.TrimPrefix(name, owner.Username+"_")); err != nil {
		return
	}
	if req.DBPassword != "" {
		if err = validDBPassword(req.DBPassword); err != nil {
			return
		}
	}
	dom, err := a.Store.GetDomainByName(ctx, req.Domain)
	if errors.Is(err, store.ErrNotFound) || (err == nil && dom.OwnerID != req.OwnerID) {
		return "", "", 0, invalid("domain not registered")
	}
	if err != nil {
		return "", "", 0, err
	}
	dbs, err := a.Store.ListDatabases(ctx)
	if err != nil {
		return "", "", 0, err
	}
	for _, d := range dbs {
		if d.Status != "error" && d.OwnerID != nil && *d.OwnerID == req.OwnerID && d.User == user {
			return "", "", 0, fmt.Errorf("%w: db user %q in use", store.ErrConflict, user)
		}
	}
	return name, user, dom.ID, nil
}
