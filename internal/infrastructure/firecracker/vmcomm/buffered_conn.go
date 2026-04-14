package vmcomm

import (
	"bufio"
	"net"
)

// bufferedConn wraps a net.Conn with a bufio.Reader so that bytes
// already buffered during the handshake are not lost.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func newBufferedConn(conn net.Conn, reader *bufio.Reader) net.Conn {
	return &bufferedConn{Conn: conn, reader: reader}
}

func (bc *bufferedConn) Read(p []byte) (int, error) {
	return bc.reader.Read(p)
}
