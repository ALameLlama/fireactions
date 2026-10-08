package agent

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/mdlayher/vsock"
)

type addressedConn struct {
	net.Conn
	peer net.Addr
}

func (c addressedConn) RemoteAddr() net.Addr { return c.peer }

type queuedListener struct {
	connections []net.Conn
	next        int
}

func (l *queuedListener) Accept() (net.Conn, error) {
	if l.next == len(l.connections) {
		return nil, net.ErrClosed
	}
	c := l.connections[l.next]
	l.next++
	return c, nil
}
func (l *queuedListener) Close() error {
	for _, c := range l.connections {
		_ = c.Close()
	}
	return nil
}
func (l *queuedListener) Addr() net.Addr { return &vsock.Addr{ContextID: vsock.Local, Port: 9001} }

func TestGuestChannelRejectsNonHostPeers(t *testing.T) {
	peers := []net.Addr{
		&vsock.Addr{ContextID: vsock.Hypervisor},
		&vsock.Addr{ContextID: vsock.Local},
		&vsock.Addr{ContextID: 3},
		&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)},
		&vsock.Addr{ContextID: vsock.Host},
	}
	listener := &queuedListener{}
	clients := make([]net.Conn, 0, len(peers))
	for _, peer := range peers {
		server, client := net.Pipe()
		listener.connections = append(listener.connections, addressedConn{Conn: server, peer: peer})
		clients = append(clients, client)
		defer client.Close()
		_ = client.SetReadDeadline(time.Now().Add(time.Second))
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := (hostOnlyListener{Listener: listener}).Accept()
		if err == nil {
			_, err = conn.Write([]byte("host-control"))
			_ = conn.Close()
		}
		done <- err
	}()
	for i, client := range clients {
		data, err := io.ReadAll(client)
		if err != nil {
			t.Fatalf("peer %d: %v", i, err)
		}
		if i == len(peers)-1 {
			if string(data) != "host-control" {
				t.Fatalf("host peer rejected: %q", data)
			}
		} else if len(data) != 0 {
			t.Fatalf("non-host peer %v received control data: %q", peers[i], data)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
