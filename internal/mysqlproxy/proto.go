// Package mysqlproxy routes MySQL/MariaDB clients to the right database by user name.
//
// The MySQL protocol speaks first (the server greets before the client says anything), so there is no SNI to route on.
// The router therefore greets with a made-up greeting, reads the user name from the client's handshake response, dials
// that user's database over TLS with the client certificate of that database's node (the only certificate it accepts),
// and then hands the client an AuthSwitchRequest carrying the database's real scramble: the client proves its password
// against the database's own challenge, and the proof is forwarded as the database's handshake response. The router never
// sees a password. After the handshake it copies bytes both ways.
package mysqlproxy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
)

const (
	capLongPassword  = 1 << 0
	capFoundRows     = 1 << 1
	capLongFlag      = 1 << 2
	capConnectWithDB = 1 << 3
	capNoSchema      = 1 << 4
	capLocalFiles    = 1 << 7
	capIgnoreSpace   = 1 << 8
	capProtocol41    = 1 << 9
	capInteractive   = 1 << 10
	capSSL           = 1 << 11
	capIgnoreSigpipe = 1 << 12
	capTransactions  = 1 << 13
	capSecureConn    = 1 << 15
	capMultiStmts    = 1 << 16
	capMultiResults  = 1 << 17
	capPSMultiRes    = 1 << 18
	capPluginAuth    = 1 << 19
	capConnectAttrs  = 1 << 20
	capLenencAuth    = 1 << 21
	capExpiredPw     = 1 << 22

	nativePlugin = "mysql_native_password"
	maxPacket    = 1 << 20 // handshake packets are tiny; anything bigger is not a handshake
)

// ourCaps is what the made-up greeting offers: no compression, no session tracking, no DEPRECATE_EOF, no query
// attributes. Every database accepts that set, so the client's choices are valid for the database too.
const ourCaps = capLongPassword | capFoundRows | capLongFlag | capConnectWithDB | capNoSchema | capLocalFiles | capIgnoreSpace |
	capProtocol41 | capInteractive | capSSL | capIgnoreSigpipe | capTransactions | capSecureConn | capMultiStmts | capMultiResults |
	capPSMultiRes | capPluginAuth | capConnectAttrs | capLenencAuth | capExpiredPw

func readPacket(r io.Reader) (seq byte, payload []byte, err error) {
	var h [4]byte
	if _, err = io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := int(h[0]) | int(h[1])<<8 | int(h[2])<<16
	if n > maxPacket {
		return 0, nil, errors.New("handshake packet too large")
	}
	payload = make([]byte, n)
	_, err = io.ReadFull(r, payload)
	return h[3], payload, err
}

func writePacket(w io.Writer, seq byte, payload []byte) error {
	n := len(payload)
	buf := append([]byte{byte(n), byte(n >> 8), byte(n >> 16), seq}, payload...)
	_, err := w.Write(buf)
	return err
}

// errPacket is an ERR packet (protocol 4.1 form).
func errPacket(code uint16, msg string) []byte {
	b := []byte{0xff, byte(code), byte(code >> 8), '#'}
	b = append(b, "28000"...)
	return append(b, msg...)
}

func scramble() []byte {
	b := make([]byte, 20)
	for i := range b {
		b[i] = byte(1 + rand.IntN(126)) // printable-safe, never NUL
	}
	return b
}

// greeting is the initial handshake packet (protocol 10) with the given 20-byte scramble.
func greeting(version string, connID uint32, caps uint32, scr []byte, plugin string) []byte {
	b := []byte{10}
	b = append(b, version...)
	b = append(b, 0)
	b = binary.LittleEndian.AppendUint32(b, connID)
	b = append(b, scr[:8]...)
	b = append(b, 0)
	b = binary.LittleEndian.AppendUint16(b, uint16(caps))
	b = append(b, 45)                          // utf8mb4_general_ci
	b = binary.LittleEndian.AppendUint16(b, 2) // SERVER_STATUS_AUTOCOMMIT
	b = binary.LittleEndian.AppendUint16(b, uint16(caps>>16))
	b = append(b, 21) // auth data length: 20 + NUL
	b = append(b, make([]byte, 10)...)
	b = append(b, scr[8:]...)
	b = append(b, 0)
	b = append(b, plugin...)
	return append(b, 0)
}

type serverGreeting struct {
	caps     uint32
	scramble []byte // the 20 bytes the password proof is computed over
	plugin   string
}

func parseGreeting(p []byte) (g serverGreeting, err error) {
	bad := errors.New("malformed greeting from the database")
	if len(p) < 1 {
		return g, bad
	}
	if p[0] == 0xff {
		return g, fmt.Errorf("the database refused the connection: %s", errText(p))
	}
	if p[0] != 10 {
		return g, bad
	}
	i := 1
	nul := indexByte(p, i, 0)
	if nul < 0 || len(p) < nul+1+4+8+1+2+1+2+2+1+10 {
		return g, bad
	}
	i = nul + 1 + 4
	part1 := p[i : i+8]
	i += 8 + 1
	lo := uint32(binary.LittleEndian.Uint16(p[i:]))
	i += 2 + 1 + 2
	hi := uint32(binary.LittleEndian.Uint16(p[i:]))
	i += 2
	g.caps = lo | hi<<16
	authLen := int(p[i])
	i += 1 + 10
	part2Len := max(13, authLen-8)
	if len(p) < i+part2Len {
		return g, bad
	}
	g.scramble = append(append([]byte{}, part1...), p[i:i+12]...) // the 13th byte is a NUL
	i += part2Len
	if g.caps&capPluginAuth != 0 {
		if end := indexByte(p, i, 0); end >= 0 {
			g.plugin = string(p[i:end])
		}
	}
	return g, nil
}

func indexByte(p []byte, from int, c byte) int {
	for i := from; i < len(p); i++ {
		if p[i] == c {
			return i
		}
	}
	return -1
}

func errText(p []byte) string {
	if len(p) > 9 && p[3] == '#' {
		return string(p[9:])
	}
	if len(p) > 3 {
		return string(p[3:])
	}
	return "error"
}

type handshakeResponse struct {
	caps, maxPacket uint32
	charset         byte
	user, db        string
	auth            []byte
	plugin          string
}

func parseResponse(p []byte) (r handshakeResponse, err error) {
	bad := errors.New("malformed handshake response")
	if len(p) < 32 {
		return r, bad
	}
	r.caps = binary.LittleEndian.Uint32(p)
	r.maxPacket = binary.LittleEndian.Uint32(p[4:])
	r.charset = p[8]
	i := 32
	end := indexByte(p, i, 0)
	if end < 0 {
		return r, bad
	}
	r.user = string(p[i:end])
	i = end + 1
	switch {
	case r.caps&capLenencAuth != 0:
		n, k := lenenc(p[i:])
		if k == 0 || i+k+int(n) > len(p) {
			return r, bad
		}
		i += k
		r.auth = p[i : i+int(n)]
		i += int(n)
	case r.caps&capSecureConn != 0:
		if i >= len(p) || i+1+int(p[i]) > len(p) {
			return r, bad
		}
		n := int(p[i])
		r.auth = p[i+1 : i+1+n]
		i += 1 + n
	default:
		end := indexByte(p, i, 0)
		if end < 0 {
			return r, bad
		}
		r.auth = p[i:end]
		i = end + 1
	}
	if r.caps&capConnectWithDB != 0 && i < len(p) {
		end := indexByte(p, i, 0)
		if end < 0 {
			return r, bad
		}
		r.db = string(p[i:end])
		i = end + 1
	}
	if r.caps&capPluginAuth != 0 && i < len(p) {
		if end := indexByte(p, i, 0); end >= 0 {
			r.plugin = string(p[i:end])
		}
	}
	return r, nil
}

func lenenc(p []byte) (n uint64, size int) {
	if len(p) == 0 {
		return 0, 0
	}
	switch {
	case p[0] < 0xfb:
		return uint64(p[0]), 1
	case p[0] == 0xfc && len(p) >= 3:
		return uint64(binary.LittleEndian.Uint16(p[1:])), 3
	case p[0] == 0xfd && len(p) >= 4:
		return uint64(p[1]) | uint64(p[2])<<8 | uint64(p[3])<<16, 4
	}
	return 0, 0
}

// backendResponse is the handshake response sent to the database: the client's choices (within what both support),
// the proof against the database's scramble (empty when the database will ask for it), no connection attributes.
func backendResponse(c handshakeResponse, dbCaps uint32, proof []byte, plugin string) []byte {
	caps := (c.caps & dbCaps & ourCaps) | capSSL | capProtocol41 | capSecureConn | capPluginAuth // only what the made-up greeting offered
	caps &^= capLenencAuth | capConnectAttrs
	if c.db == "" {
		caps &^= capConnectWithDB
	} else {
		caps |= capConnectWithDB
	}
	b := binary.LittleEndian.AppendUint32(nil, caps)
	b = binary.LittleEndian.AppendUint32(b, c.maxPacket)
	b = append(b, c.charset)
	b = append(b, make([]byte, 23)...)
	b = append(append(b, c.user...), 0)
	b = append(append(b, byte(len(proof))), proof...)
	if c.db != "" {
		b = append(append(b, c.db...), 0)
	}
	return append(append(b, plugin...), 0)
}

// backendSSLRequest is the 32-byte packet that makes the database switch to TLS.
func backendSSLRequest(c handshakeResponse, dbCaps uint32) []byte {
	caps := (c.caps & dbCaps & ourCaps) | capSSL | capProtocol41 | capSecureConn | capPluginAuth // only what the made-up greeting offered
	caps &^= capLenencAuth | capConnectAttrs
	b := binary.LittleEndian.AppendUint32(nil, caps)
	b = binary.LittleEndian.AppendUint32(b, c.maxPacket)
	b = append(b, c.charset)
	return append(b, make([]byte, 23)...)
}
