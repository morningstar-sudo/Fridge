// Package scram implements SCRAM-SHA-256 (RFC 5802 / RFC 7677) without channel
// binding. Usernames must not contain ',' or '=' (rejected, no escaping).
package scram

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	ErrProof  = errors.New("scram: invalid proof")
	ErrFormat = errors.New("scram: malformed message")
)

const gs2 = "n,,"

// Bounds the client accepts for the server-supplied iteration count.
const (
	MinIterations = 4096
	MaxIterations = 10000000
)

func mac(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

// SaltedPassword is PBKDF2-HMAC-SHA-256 with a 32 byte output.
func SaltedPassword(password string, salt []byte, iter int) []byte {
	k, err := pbkdf2.Key(sha256.New, password, salt, iter, 32)
	if err != nil { // only for invalid params (iter <= 0); callers validate
		panic(err)
	}
	return k
}

// Keys derives StoredKey and ServerKey from the salted password.
func Keys(salted []byte) (storedKey, serverKey []byte) {
	ck := mac(salted, "Client Key")
	sk := sha256.Sum256(ck)
	return sk[:], mac(salted, "Server Key")
}

// attrs parses "k=v,k=v,..." requiring exactly the given keys in order.
func attrs(msg string, keys ...string) ([]string, error) {
	parts := strings.Split(msg, ",")
	if len(parts) != len(keys) {
		return nil, ErrFormat
	}
	vals := make([]string, len(keys))
	for i, p := range parts {
		if len(p) < 2 || p[0] != keys[i][0] || p[1] != '=' {
			return nil, ErrFormat
		}
		vals[i] = p[2:]
	}
	return vals, nil
}

type Server struct{ ClientFirstBare, ServerFirst, Nonce string }

// StartServer parses client-first (gs2 header must be "n,,") and builds server-first.
func StartServer(clientFirst string, salt []byte, iter int, serverNonce string) (*Server, string, error) {
	bare, ok := strings.CutPrefix(clientFirst, gs2)
	if !ok || iter <= 0 {
		return nil, "", ErrFormat
	}
	v, err := attrs(bare, "n", "r")
	if err != nil || v[0] == "" || v[1] == "" || strings.Contains(v[0], "=") {
		return nil, "", ErrFormat
	}
	nonce := v[1] + serverNonce
	return &Server{
		ClientFirstBare: bare,
		Nonce:           nonce,
		ServerFirst:     fmt.Sprintf("r=%s,s=%s,i=%d", nonce, base64.StdEncoding.EncodeToString(salt), iter),
	}, v[0], nil
}

// Finish verifies the client proof and returns server-final.
func (s *Server) Finish(clientFinal string, storedKey, serverKey []byte) (string, error) {
	i := strings.LastIndex(clientFinal, ",p=")
	if i < 0 {
		return "", ErrFormat
	}
	withoutProof := clientFinal[:i]
	v, err := attrs(withoutProof, "c", "r")
	if err != nil || v[0] != "biws" {
		return "", ErrFormat
	}
	if subtle.ConstantTimeCompare([]byte(v[1]), []byte(s.Nonce)) != 1 {
		return "", ErrProof
	}
	proof, err := base64.StdEncoding.DecodeString(clientFinal[i+3:])
	if err != nil || len(proof) != sha256.Size {
		return "", ErrFormat
	}
	authMsg := s.ClientFirstBare + "," + s.ServerFirst + "," + withoutProof
	sig := mac(storedKey, authMsg)
	for j := range sig {
		sig[j] ^= proof[j] // now ClientKey
	}
	got := sha256.Sum256(sig)
	if subtle.ConstantTimeCompare(got[:], storedKey) != 1 {
		return "", ErrProof
	}
	return "v=" + base64.StdEncoding.EncodeToString(mac(serverKey, authMsg)), nil
}

type Client struct {
	user, password, nonce string
	authMsg               string
	serverKey             []byte
}

func NewClient(username, password, clientNonce string) *Client {
	return &Client{user: username, password: password, nonce: clientNonce}
}

func (c *Client) bare() string { return "n=" + c.user + ",r=" + c.nonce }

func (c *Client) First() string { return gs2 + c.bare() }

// Final computes client-final from server-first, deriving the salted password.
func (c *Client) Final(serverFirst string) (string, error) {
	return c.final(serverFirst, nil)
}

// FinalWithSalted is Final with a precomputed salted password.
func (c *Client) FinalWithSalted(serverFirst string, salted []byte) (string, error) {
	return c.final(serverFirst, salted)
}

func (c *Client) final(serverFirst string, salted []byte) (string, error) {
	v, err := attrs(serverFirst, "r", "s", "i")
	if err != nil || !strings.HasPrefix(v[0], c.nonce) || len(v[0]) == len(c.nonce) {
		return "", ErrFormat
	}
	salt, err := base64.StdEncoding.DecodeString(v[1])
	iter, err2 := strconv.Atoi(v[2])
	if err != nil || err2 != nil || strings.Trim(v[2], "0123456789") != "" ||
		iter < MinIterations || iter > MaxIterations ||
		strings.ContainsAny(c.user, ",=") {
		return "", ErrFormat
	}
	if salted == nil {
		salted = SaltedPassword(c.password, salt, iter)
	}
	stored, serverKey := Keys(salted)
	withoutProof := "c=biws,r=" + v[0]
	c.authMsg = c.bare() + "," + serverFirst + "," + withoutProof
	c.serverKey = serverKey
	sig := mac(stored, c.authMsg)
	ck := mac(salted, "Client Key")
	for j := range sig {
		sig[j] ^= ck[j]
	}
	return withoutProof + ",p=" + base64.StdEncoding.EncodeToString(sig), nil
}

// Verify checks the server signature in server-final.
func (c *Client) Verify(serverFinal string) error {
	v, ok := strings.CutPrefix(serverFinal, "v=")
	if !ok || c.serverKey == nil {
		return ErrFormat
	}
	got, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return ErrFormat
	}
	if subtle.ConstantTimeCompare(got, mac(c.serverKey, c.authMsg)) != 1 {
		return ErrProof
	}
	return nil
}
