package handlers

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/centrys-labs/stagezero/gen"
	"github.com/centrys-labs/stagezero/ssh"
)

// The probe failure code drives which remedy the client shows. Host-key trust
// failures must not be reported as a rejected SSH key, or operators are sent to
// fix the wrong thing.
func TestClassifyDialError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want gen.ErrorResponseCode
	}{
		{"invalid private key", fmt.Errorf("parse: %w", ssh.ErrInvalidPrivateKey), gen.KeyInvalid},
		{"untrusted host key", fmt.Errorf("Cannot connect to host:22: %w", ssh.ErrHostKeyUntrusted), gen.HostKeyUntrusted},
		{"network failure", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, gen.Unreachable},
		{"key refused", errors.New("ssh: handshake failed"), gen.KeyRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyDialError(tc.err); got.code != tc.want {
				t.Fatalf("code = %q, want %q", got.code, tc.want)
			}
		})
	}
}
