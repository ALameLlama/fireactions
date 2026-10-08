package agent

import (
	"net"

	"github.com/mdlayher/vsock"
)

// hostOnlyListener admits the Firecracker host's CID, never a guest/local peer.
// Socket authorization on the host remains the authentication boundary.
type hostOnlyListener struct {
	net.Listener
}

func (l hostOnlyListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		peer, ok := conn.RemoteAddr().(*vsock.Addr)
		if ok && peer.ContextID == vsock.Host {
			return conn, nil
		}
		_ = conn.Close()
	}
}
