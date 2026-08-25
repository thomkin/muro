package proxy

import (
	"encoding/binary"
	"errors"
)

// ErrNoSNI is returned when a ClientHello was successfully parsed but
// contained no server_name extension (e.g. a client connecting by bare IP,
// or one that hasn't finished sending its ClientHello yet).
var ErrNoSNI = errors.New("proxy: no SNI extension in ClientHello")

// ExtractSNI parses a raw TLS record containing (at least the start of) a
// ClientHello handshake message — captured directly off the wire, before
// any TLS termination; this package never terminates TLS — and returns the
// SNI hostname from its server_name extension.
//
// This is a direct, hand-rolled parse of the TLS record/handshake/
// extension framing (RFC 8446 §4, RFC 6066 §3) rather than using
// crypto/tls, because crypto/tls has no exported API for parsing a
// ClientHello from a byte slice without also driving a real handshake over
// a net.Conn — and the whole point here (DESIGN.md §6.2/§14) is to inspect
// the cleartext SNI field without ever performing or terminating a TLS
// handshake at all.
func ExtractSNI(clientHello []byte) (string, error) {
	// TLS record header: type(1) + legacy_record_version(2) + length(2).
	if len(clientHello) < 5 {
		return "", errors.New("proxy: too short for a TLS record header")
	}
	if clientHello[0] != 0x16 {
		return "", errors.New("proxy: not a TLS handshake record")
	}
	recLen := int(binary.BigEndian.Uint16(clientHello[3:5]))
	body := clientHello[5:]
	if len(body) < recLen {
		return "", errors.New("proxy: truncated TLS record")
	}
	body = body[:recLen]

	// Handshake header: msg_type(1) + length(3, big-endian).
	if len(body) < 4 {
		return "", errors.New("proxy: truncated handshake header")
	}
	if body[0] != 0x01 {
		return "", errors.New("proxy: not a ClientHello handshake message")
	}
	hsLen := int(body[1])<<16 | int(body[2])<<8 | int(body[3])
	hello := body[4:]
	if len(hello) < hsLen {
		return "", errors.New("proxy: truncated ClientHello body")
	}
	hello = hello[:hsLen]

	// client_version(2) + random(32).
	if len(hello) < 34 {
		return "", errors.New("proxy: truncated ClientHello (version/random)")
	}
	p := hello[34:]

	// session_id: length(1) + id.
	if len(p) < 1 {
		return "", errors.New("proxy: truncated ClientHello (session id length)")
	}
	sidLen := int(p[0])
	p = p[1:]
	if len(p) < sidLen {
		return "", errors.New("proxy: truncated ClientHello (session id)")
	}
	p = p[sidLen:]

	// cipher_suites: length(2) + suites.
	if len(p) < 2 {
		return "", errors.New("proxy: truncated ClientHello (cipher suites length)")
	}
	csLen := int(binary.BigEndian.Uint16(p[:2]))
	p = p[2:]
	if len(p) < csLen {
		return "", errors.New("proxy: truncated ClientHello (cipher suites)")
	}
	p = p[csLen:]

	// compression_methods: length(1) + methods.
	if len(p) < 1 {
		return "", errors.New("proxy: truncated ClientHello (compression methods length)")
	}
	cmLen := int(p[0])
	p = p[1:]
	if len(p) < cmLen {
		return "", errors.New("proxy: truncated ClientHello (compression methods)")
	}
	p = p[cmLen:]

	// extensions are optional — a ClientHello with nothing left has none.
	if len(p) == 0 {
		return "", ErrNoSNI
	}
	if len(p) < 2 {
		return "", errors.New("proxy: truncated ClientHello (extensions length)")
	}
	extLen := int(binary.BigEndian.Uint16(p[:2]))
	p = p[2:]
	if len(p) < extLen {
		return "", errors.New("proxy: truncated ClientHello (extensions)")
	}
	p = p[:extLen]

	for len(p) >= 4 {
		extType := binary.BigEndian.Uint16(p[0:2])
		extDataLen := int(binary.BigEndian.Uint16(p[2:4]))
		p = p[4:]
		if len(p) < extDataLen {
			return "", errors.New("proxy: truncated extension data")
		}
		extData := p[:extDataLen]
		p = p[extDataLen:]

		if extType != 0x0000 { // server_name
			continue
		}
		return parseServerNameExtension(extData)
	}
	return "", ErrNoSNI
}

// parseServerNameExtension parses RFC 6066 §3's server_name extension body:
// server_name_list_length(2) + a list of (name_type(1), name_length(2),
// name) entries. Returns the first host_name (type 0) entry found.
func parseServerNameExtension(extData []byte) (string, error) {
	if len(extData) < 2 {
		return "", errors.New("proxy: truncated SNI extension")
	}
	listLen := int(binary.BigEndian.Uint16(extData[:2]))
	list := extData[2:]
	if len(list) < listLen {
		return "", errors.New("proxy: truncated SNI server name list")
	}
	list = list[:listLen]

	for len(list) >= 3 {
		nameType := list[0]
		nameLen := int(binary.BigEndian.Uint16(list[1:3]))
		list = list[3:]
		if len(list) < nameLen {
			return "", errors.New("proxy: truncated SNI host_name entry")
		}
		name := list[:nameLen]
		list = list[nameLen:]
		if nameType == 0x00 { // host_name
			return string(name), nil
		}
	}
	return "", ErrNoSNI
}
