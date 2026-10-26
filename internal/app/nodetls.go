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
	life := a.CA.Remaining(nodeTLSValidity)
	if life <= 0 {
		return errors.New("the CA is about to expire; renew it before creating databases")
	}
	sc, sk, err := a.CA.IssueNames([]string{d.Domain, swarm.DBService(d.NodeKey)}, life) // both names: see store.Route
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
	return a.Store.SetDBTLS(ctx, d.ID, store.DBTLS{ServerCertPEM: string(sc), ServerKeyEnc: skEnc, ClientCertPEM: string(cc), ClientKeyEnc: ckEnc, NotAfter: notAfter})
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
		CA: string(a.CA.CertPEM()), ServerName: swarm.DBService(d.NodeKey)}, nil
}

// edgeEnv is what the sidecar needs to speak TLS to its database.
func edgeEnv(t *engine.TLS) []string {
	return []string{"FRIDGE_TLS_CERT=" + t.ClientCert, "FRIDGE_TLS_KEY=" + t.ClientKey, "FRIDGE_TLS_CA=" + t.CA}
}
