package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"fridge/internal/engine"
	"fridge/internal/pki"
	"fridge/internal/store"
	"fridge/internal/swarm"
)

// Every database shares one internal network, so what keeps one database away from another is not the network but
// the database itself: it accepts a connection only from a client holding the node's own certificate (and then the
// password). That certificate lives in the node's edge sidecar and in the jobs that work on this node, nowhere else.
// The server certificate lets those clients check they reached the right database.
const nodeTLSValidity = 5 * 365 * 24 * time.Hour

// issueNodeTLS creates the node's two certificates and stores them (keys encrypted).
func (a *App) issueNodeTLS(ctx context.Context, d store.Database) error {
	ca, err := a.signingCA(ctx, d)
	if err != nil {
		return err
	}
	life := ca.Remaining(nodeTLSValidity)
	if life <= 0 {
		return errors.New("the CA is about to expire; renew it before creating databases")
	}
	sc, sk, err := ca.IssueNames([]string{d.Domain, swarm.DBService(d.NodeKey)}, life) // both names: see store.Route
	if err != nil {
		return err
	}
	cc, ck, err := pki.SelfSignedClient(swarm.EdgeService(d.NodeKey), life)
	if err != nil {
		return err
	}
	skEnc, err := a.Box.Encrypt(string(sk))
	if err != nil {
		return err
	}
	ckEnc, err := a.Box.Encrypt(string(ck))
	if err != nil {
		return err
	}
	notAfter, err := certNotAfter(sc)
	if err != nil {
		return err
	}
	return a.Store.SetDBTLS(ctx, d.ID, store.DBTLS{ServerCertPEM: string(sc), ServerKeyEnc: skEnc, ClientCertPEM: string(cc), ClientKeyEnc: ckEnc, NotAfter: notAfter, CACertPEM: string(ca.CertPEM())})
}

// nodeTLS is the node's TLS material in the clear. A node made before this existed has none.
func (a *App) nodeTLS(ctx context.Context, d store.Database) (*engine.TLS, error) {
	t, err := a.Store.GetDBTLS(ctx, d.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("database %s has no node certificate", d.Name)
	} else if err != nil {
		return nil, err
	}
	sk, err := a.Box.Decrypt(t.ServerKeyEnc)
	if err != nil {
		return nil, err
	}
	ck, err := a.Box.Decrypt(t.ClientKeyEnc)
	if err != nil {
		return nil, err
	}
	return &engine.TLS{ServerCert: t.ServerCertPEM, ServerKey: sk, ClientCert: t.ClientCertPEM, ClientKey: ck,
		CA: caPEM(t, a.CA), ServerName: swarm.DBService(d.NodeKey)}, nil
}

// edgeEnv is what the sidecar needs to speak TLS to its database.
func edgeEnv(t *engine.TLS) []string {
	return []string{"FRIDGE_TLS_CERT=" + t.ClientCert, "FRIDGE_TLS_KEY=" + t.ClientKey, "FRIDGE_TLS_CA=" + t.CA}
}

// caPEM is the CA that signed the node's server certificate: the tenant's, or the instance-wide CA for a node made
// before tenant CAs existed.
func caPEM(t store.DBTLS, instance *pki.CA) string {
	if t.CACertPEM != "" {
		return t.CACertPEM
	}
	return string(instance.CertPEM())
}

// tenantCA is the CA of one user account, created (and stored, its key encrypted) the first time it is needed. The
// certificates of that user's databases are signed by it: the CA they download vouches for their databases only.
func (a *App) tenantCA(ctx context.Context, ownerID int64) (*pki.CA, error) {
	rec, err := a.Store.GetTenantCA(ctx, ownerID)
	if errors.Is(err, store.ErrNotFound) {
		u, err := a.Store.GetUser(ctx, ownerID)
		if err != nil {
			return nil, err
		}
		certPEM, keyPEM, err := pki.NewCA("Fridge CA - " + u.Username)
		if err != nil {
			return nil, err
		}
		enc, err := a.Box.Encrypt(string(keyPEM))
		if err != nil {
			return nil, err
		}
		if rec, err = a.Store.CreateTenantCA(ctx, store.TenantCA{UserID: ownerID, CertPEM: string(certPEM), KeyEnc: enc}); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	key, err := a.Box.Decrypt(rec.KeyEnc)
	if err != nil {
		return nil, err
	}
	return pki.Parse([]byte(rec.CertPEM), []byte(key))
}

// signingCA is the CA that signs d's certificates: its owner's, or the instance CA for a database without an owner.
func (a *App) signingCA(ctx context.Context, d store.Database) (*pki.CA, error) {
	if d.OwnerID == nil {
		return a.CA, nil
	}
	return a.tenantCA(ctx, *d.OwnerID)
}

// DatabaseCA is the CA certificate (PEM) a client trusts to verify d's server certificate.
func (a *App) DatabaseCA(ctx context.Context, d store.Database) (string, error) {
	t, err := a.Store.GetDBTLS(ctx, d.ID)
	if errors.Is(err, store.ErrNotFound) {
		return string(a.CA.CertPEM()), nil // a node without node TLS is served by the instance CA's route certificate
	} else if err != nil {
		return "", err
	}
	return caPEM(t, a.CA), nil
}
