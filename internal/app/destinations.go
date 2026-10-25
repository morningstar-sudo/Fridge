package app

import (
	"context"
	"errors"
	"net/url"

	"github.com/robfig/cron/v3"

	"fridge/internal/objstore"
	"fridge/internal/store"
)

func validDestination(d store.Destination) error {
	if d.Name == "" {
		return invalid("name must not be empty")
	}
	if u, err := url.Parse(d.Endpoint); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return invalid("endpoint must be an http(s) URL")
	}
	if d.Bucket == "" {
		return invalid("bucket must not be empty")
	}
	return nil
}

func (a *App) CreateDestination(ctx context.Context, d store.Destination, secretKey string) (store.Destination, error) {
	if err := validDestination(d); err != nil {
		return store.Destination{}, err
	}
	if d.Provider == "" {
		d.Provider = "s3"
	}
	enc, err := a.Box.Encrypt(secretKey)
	if err != nil {
		return store.Destination{}, err
	}
	d.SecretKeyEnc = enc
	if err := a.Store.CreateDestination(ctx, &d); err != nil {
		return store.Destination{}, err
	}
	return d, nil
}

// UpdateDestination replaces d; an empty secretKey keeps the stored one.
func (a *App) UpdateDestination(ctx context.Context, d store.Destination, secretKey string) error {
	if err := validDestination(d); err != nil {
		return err
	}
	old, err := a.Store.GetDestination(ctx, d.ID)
	if err != nil {
		return err
	}
	if old.DeletedAt != nil {
		return invalid("destination deleted")
	}
	d.SecretKeyEnc = old.SecretKeyEnc
	if secretKey != "" {
		if d.SecretKeyEnc, err = a.Box.Encrypt(secretKey); err != nil {
			return err
		}
	}
	if d.Provider == "" {
		d.Provider = old.Provider
	}
	return a.Store.UpdateDestination(ctx, d)
}

func (a *App) DeleteDestination(ctx context.Context, id int64) error {
	return a.Store.DeleteDestination(ctx, id)
}

// objects opens the object store of destination id.
func (a *App) objects(ctx context.Context, id int64) (ObjectStore, store.Destination, error) {
	d, err := a.Store.GetDestination(ctx, id)
	if err != nil {
		return nil, d, err
	}
	if d.DeletedAt != nil {
		return nil, d, invalid("destination deleted")
	}
	sk, err := a.Box.Decrypt(d.SecretKeyEnc)
	if err != nil {
		return nil, d, err
	}
	g, err := a.guardFor(ctx, d)
	if err != nil {
		return nil, d, err
	}
	if a.OpenObjects != nil {
		o, err := a.OpenObjects(d, sk, g)
		return o, d, err
	}
	o, err := objstore.New(d, sk, g)
	if err != nil {
		return nil, d, err
	}
	return o, d, nil
}

// guardFor is the SSRF guard for d: none when an admin owns it. An admin who is demoted
// has their destinations guarded from then on.
func (a *App) guardFor(ctx context.Context, d store.Destination) (*objstore.Guard, error) {
	if d.OwnerID != nil {
		owner, err := a.Store.GetUser(ctx, *d.OwnerID)
		if err != nil {
			return nil, err
		}
		if owner.Role == "admin" {
			return nil, nil
		}
	}
	return a.endpointGuard(ctx)
}

func (a *App) TestDestination(ctx context.Context, id int64) error {
	o, _, err := a.objects(ctx, id)
	if err != nil {
		return err
	}
	return o.Test(ctx)
}

func (a *App) ListObjects(ctx context.Context, destID int64, prefix, q string) ([]objstore.Object, bool, error) {
	o, _, err := a.objects(ctx, destID)
	if err != nil {
		return nil, false, err
	}
	return o.List(ctx, prefix, q, 200)
}

func (a *App) validSchedule(ctx context.Context, sc *store.Schedule) error {
	if _, err := cron.ParseStandard(sc.Cron); err != nil {
		return invalid("cron: %v", err)
	}
	if sc.KeepLatest < 1 {
		return invalid("keep_latest must be >= 1")
	}
	if sc.Prefix == "" {
		sc.Prefix = "fridge"
	}
	dest, err := a.Store.GetDestination(ctx, sc.DestinationID)
	if errors.Is(err, store.ErrNotFound) {
		return invalid("destination %d does not exist", sc.DestinationID)
	} else if err != nil {
		return err
	}
	if dest.DeletedAt != nil {
		return invalid("destination deleted")
	}
	return nil
}

func (a *App) reload(ctx context.Context) error {
	if a.Sched == nil {
		return nil
	}
	return a.Sched.Reload(ctx)
}

func (a *App) CreateSchedule(ctx context.Context, sc store.Schedule) (store.Schedule, error) {
	if err := a.validSchedule(ctx, &sc); err != nil {
		return store.Schedule{}, err
	}
	if _, err := a.Store.GetDatabase(ctx, sc.DatabaseID); errors.Is(err, store.ErrNotFound) {
		return store.Schedule{}, invalid("database %d does not exist", sc.DatabaseID)
	} else if err != nil {
		return store.Schedule{}, err
	}
	if err := a.Store.CreateSchedule(ctx, &sc); err != nil {
		return store.Schedule{}, err
	}
	return sc, a.reload(ctx)
}

func (a *App) UpdateSchedule(ctx context.Context, sc store.Schedule) error {
	if err := a.validSchedule(ctx, &sc); err != nil {
		return err
	}
	if err := a.Store.UpdateSchedule(ctx, sc); err != nil {
		return err
	}
	return a.reload(ctx)
}

func (a *App) DeleteSchedule(ctx context.Context, id int64) error {
	if err := a.Store.DeleteSchedule(ctx, id); err != nil {
		return err
	}
	return a.reload(ctx)
}
