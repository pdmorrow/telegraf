package chrony

import (
	"fmt"
	"net"
	"testing"

	"github.com/influxdata/telegraf/testutil"
)

func validate_request(t *testing.T, req_len int, req []byte) {
	if req_len != 104 {
		t.Fatal(fmt.Errorf("expected request length of %d bytes", 104))
	}

	ver := req[0]
}

func readAndWriteDataUDP(t *testing.T, conn *net.UDPConn) error {
	rb := make([]byte, 104)

	n, _, err := conn.ReadFromUDP(rb)
	if err != nil {
		t.Fatal(err)
	}

	validate_request(t, n, rb)

	return nil
}

// TODO: override server socket with something we create here, listen on
// that socket and send the data back.
func TestDomainSocketGather(t *testing.T) {
}

func TestV4SocketGather(t *testing.T) {
	c := Chrony{
		DNSLookup: true,
		UseSocket: true,
	}

	socket_type = CHRONY_SOCKET_V4UDP
	server_v4_addr = "127.0.0.1:1323"

	la, err := net.ResolveUDPAddr("udp4", server_v4_addr)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.ListenUDP("udp4", la)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	go readAndWriteDataUDP(t, conn)

	var acc testutil.Accumulator
	err = c.Gather(&acc)
	if err != nil {
		t.Fatal(err)
	}
}

/*
func TestV6SocketGather(t *testing.T) {
	c := Chrony{
		DNSLookup: true,
		UseSocket: true,
	}

	socket_type = CHRONY_SOCKET_V6UDP
	server_v6_addr = "[::1]:1323"

	la, err := net.ResolveUDPAddr("udp6", server_v6_addr)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.ListenUDP("udp6", la)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	go readAndWriteDataUDP(t, conn)

	var acc testutil.Accumulator
	err = c.Gather(&acc)
	if err != nil {
		t.Fatal(err)
	}
}
*/
