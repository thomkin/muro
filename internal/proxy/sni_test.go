package proxy

import (
	"crypto/tls"
	"net"
	"testing"
	"time"
)

// TestExtractSNI_RealClientHello drives a real crypto/tls client handshake
// over a net.Pipe and parses the actual ClientHello bytes it sends — this
// exercises the parser against real, correctly-framed TLS wire data rather
// than a hand-invented format.
func TestExtractSNI_RealClientHello(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	go func() {
		conf := &tls.Config{ServerName: "example.com", InsecureSkipVerify: true} //nolint:gosec // test only, never a real handshake
		_ = tls.Client(clientConn, conf).Handshake()                             // expected to fail/hang; we only need the ClientHello it sends
	}()

	if err := serverConn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	var all []byte
	buf := make([]byte, 4096)
	for {
		n, err := serverConn.Read(buf)
		if n > 0 {
			all = append(all, buf[:n]...)
			if host, perr := ExtractSNI(all); perr == nil {
				if host != "example.com" {
					t.Errorf("ExtractSNI = %q, want %q", host, "example.com")
				}
				return
			}
		}
		if err != nil {
			t.Fatalf("read stopped before a full ClientHello arrived (%d bytes so far): %v", len(all), err)
		}
	}
}

func TestExtractSNI_TooShort(t *testing.T) {
	if _, err := ExtractSNI([]byte{0x16, 0x03}); err == nil {
		t.Error("expected an error for a too-short input")
	}
}

func TestExtractSNI_NotAHandshakeRecord(t *testing.T) {
	// content type 0x17 = application data, not 0x16 = handshake.
	rec := []byte{0x17, 0x03, 0x03, 0x00, 0x02, 0xAA, 0xBB}
	if _, err := ExtractSNI(rec); err == nil {
		t.Error("expected an error for a non-handshake record")
	}
}

func TestExtractSNI_NotAClientHello(t *testing.T) {
	// A well-formed record header, but handshake msg_type 0x02
	// (ServerHello) instead of 0x01 (ClientHello).
	rec := []byte{0x16, 0x03, 0x03, 0x00, 0x04, 0x02, 0x00, 0x00, 0x00}
	if _, err := ExtractSNI(rec); err == nil {
		t.Error("expected an error for a non-ClientHello handshake message")
	}
}

// TestExtractSNI_NoExtensions builds a minimal, syntactically valid
// ClientHello with zero bytes left after compression_methods (no
// extensions block at all) and confirms it reports ErrNoSNI rather than a
// parse error.
func TestExtractSNI_NoExtensions(t *testing.T) {
	// ClientHello body: version(2) + random(32) + session_id_len(1)=0
	// + cipher_suites_len(2)=0 + compression_methods_len(1)=0.
	body := make([]byte, 0, 64)
	body = append(body, 0x03, 0x03)          // client_version
	body = append(body, make([]byte, 32)...) // random
	body = append(body, 0x00)                // session_id length = 0
	body = append(body, 0x00, 0x00)          // cipher_suites length = 0
	body = append(body, 0x00)                // compression_methods length = 0
	// no extensions field at all

	hs := make([]byte, 0, len(body)+4)
	hs = append(hs, 0x01) // ClientHello
	hs = append(hs, byte(len(body)>>16), byte(len(body)>>8), byte(len(body)))
	hs = append(hs, body...)

	rec := make([]byte, 0, len(hs)+5)
	rec = append(rec, 0x16, 0x03, 0x03)
	rec = append(rec, byte(len(hs)>>8), byte(len(hs)))
	rec = append(rec, hs...)

	_, err := ExtractSNI(rec)
	if err != ErrNoSNI {
		t.Errorf("ExtractSNI error = %v, want ErrNoSNI", err)
	}
}
