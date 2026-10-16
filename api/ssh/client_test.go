package ssh

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// startFollowServer runs an in-process SSH server whose only command writes one
// line and then never exits, the way "docker logs -f" behaves. It is the only
// way to exercise the cancellation path: the failure it guards against is a
// stream that keeps running after the caller is gone, which no amount of
// checking a returned value would catch.
//
// Returns the address to dial and a client private key the server accepts.
func startFollowServer(t *testing.T) (addr string, clientKeyPEM string, hostKey gossh.PublicKey) {
	t.Helper()

	hostKeyPEM, err := GenerateEd25519Key()
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	hostSigner, err := gossh.ParsePrivateKey([]byte(hostKeyPEM))
	if err != nil {
		t.Fatalf("parse host key: %v", err)
	}

	clientKeyPEM, err = GenerateEd25519Key()
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}

	config := &gossh.ServerConfig{
		PublicKeyCallback: func(gossh.ConnMetadata, gossh.PublicKey) (*gossh.Permissions, error) {
			return nil, nil // the test's own key is the only one that can reach this
		},
	}
	config.AddHostKey(hostSigner)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return // listener closed by cleanup
			}
			go serveFollowConn(conn, config)
		}
	}()

	return listener.Addr().String(), clientKeyPEM, hostSigner.PublicKey()
}

func trustedClientConfig(t *testing.T, addr, key string, hostKey gossh.PublicKey) ServerConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(knownhosts.Line([]string{knownhosts.Normalize(addr)}, hostKey)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_KNOWN_HOSTS_FILE", path)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	portNum, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return ServerConfig{Host: host, Port: portNum, User: "tester", PrivateKey: key}
}

func TestNewClientVerifiesHostKey(t *testing.T) {
	addr, clientKey, hostKey := startFollowServer(t)
	cfg := trustedClientConfig(t, addr, clientKey, hostKey)
	path := os.Getenv("SSH_KNOWN_HOSTS_FILE")

	t.Run("trusted", func(t *testing.T) {
		client, err := NewClient(cfg)
		if err != nil {
			t.Fatal(err)
		}
		client.Close()
	})
	t.Run("missing file", func(t *testing.T) {
		t.Setenv("SSH_KNOWN_HOSTS_FILE", "")
		if client, err := NewClient(cfg); err == nil {
			client.Close()
			t.Fatal("missing trust store accepted")
		}
	})
	t.Run("unknown host", func(t *testing.T) {
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		client, err := NewClient(cfg)
		if err == nil {
			client.Close()
			t.Fatal("unknown host accepted")
		}
		if !errors.Is(err, ErrHostKeyUntrusted) {
			t.Fatalf("unknown host error not classified: %v", err)
		}
	})
	t.Run("mismatched key", func(t *testing.T) {
		otherKeyPEM, err := GenerateEd25519Key()
		if err != nil {
			t.Fatal(err)
		}
		otherSigner, err := gossh.ParsePrivateKey([]byte(otherKeyPEM))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(knownhosts.Line([]string{knownhosts.Normalize(addr)}, otherSigner.PublicKey())+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		client, err := NewClient(cfg)
		if err == nil {
			client.Close()
			t.Fatal("mismatched host key accepted")
		}
		if !errors.Is(err, ErrHostKeyUntrusted) {
			t.Fatalf("mismatched host key error not classified: %v", err)
		}
	})
	t.Run("rotation after verification", func(t *testing.T) {
		if err := os.WriteFile(path, []byte(knownhosts.Line([]string{knownhosts.Normalize(addr)}, hostKey)+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		client, err := NewClient(cfg)
		if err != nil {
			t.Fatal(err)
		}
		client.Close()
	})
	t.Run("connection failure", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		listener.Close()
		cfg.Port = port
		if client, err := NewClient(cfg); err == nil {
			client.Close()
			t.Fatal("closed server accepted")
		}
	})
}

func serveFollowConn(conn net.Conn, config *gossh.ServerConfig) {
	defer conn.Close()

	sshConn, chans, reqs, err := gossh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go gossh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			newChannel.Reject(gossh.UnknownChannelType, "only sessions")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			return
		}

		go func() {
			for req := range requests {
				if req.Type != "exec" {
					req.Reply(false, nil)
					continue
				}
				req.Reply(true, nil)
				channel.Write([]byte("first line\n"))
				// Then hang, like a follow command does. The only thing that ends
				// this is the client tearing the session down.
			}
		}()
	}
}

// A cancelled context has to actually stop the stream. Without it every viewer
// who navigates away from a log tab strands a goroutine, an SSH session, and a
// remote process — a leak that only shows up under load, long after the change
// that caused it.
func TestStreamCommandContextCancelStopsStream(t *testing.T) {
	addr, clientKey, hostKey := startFollowServer(t)
	trustedClientConfig(t, addr, clientKey, hostKey)

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	portNum, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}

	client, err := NewClient(ServerConfig{
		Host:       host,
		Port:       portNum,
		User:       "tester",
		PrivateKey: clientKey,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	stdout, _, done := client.StreamCommandContext(ctx, "docker logs -f whatever")

	select {
	case line, ok := <-stdout:
		if !ok {
			t.Fatal("stdout closed before any output")
		}
		if line != "first line" {
			t.Fatalf("got %q, want %q", line, "first line")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first line")
	}

	// The command is still running and nobody is reading stdout any more, which
	// is exactly the state a disconnected viewer leaves behind.
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("done = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not stop after the context was cancelled")
	}

	select {
	case _, ok := <-stdout:
		if ok {
			t.Error("stdout still open after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stdout was never closed after cancellation")
	}
}
