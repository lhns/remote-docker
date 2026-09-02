// Command udpecho answers every datagram with the same bytes, and can send one.
//
// Both ends of the one test that can prove UDP crosses the tunnel: something
// in the container has to answer, and something here has to ask (ADR 0038).
// Nothing in a stock alpine image does either, and the same binary at both
// ends behaves the same everywhere.
//
//	udpecho :5353                             answer datagrams
//	udpecho send 127.0.0.1:15353 hello        send one and print the answer
package main

import (
	"fmt"
	"net"
	"os"
	"time"
)

// sendTimeout bounds the send mode. Long enough for a datagram to cross the
// tunnel and come back, short enough that a test which is going to fail does
// not sit there.
const sendTimeout = 3 * time.Second

func main() {
	if len(os.Args) > 1 && os.Args[1] == "send" {
		if len(os.Args) != 4 {
			fail("usage: udpecho send <addr> <message>")
		}
		send(os.Args[2], os.Args[3])
		return
	}

	addr := ":5353"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}
	serve(addr)
}

func serve(addr string) {
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		fail(err.Error())
	}
	defer func() { _ = conn.Close() }()

	fmt.Println("udpecho listening on", conn.LocalAddr())

	// 65535 is every datagram there can be, so nothing is truncated into a
	// reply that looks like the request and is not.
	buf := make([]byte, 65535)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			fail(err.Error())
		}
		if _, err := conn.WriteTo(buf[:n], from); err != nil {
			fmt.Fprintln(os.Stderr, "udpecho:", err)
		}
	}
}

func send(addr, message string) {
	conn, err := net.Dial("udp", addr)
	if err != nil {
		fail(err.Error())
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte(message)); err != nil {
		fail(err.Error())
	}
	if err := conn.SetReadDeadline(time.Now().Add(sendTimeout)); err != nil {
		fail(err.Error())
	}

	buf := make([]byte, 65535)
	n, err := conn.Read(buf)
	if err != nil {
		fail(err.Error())
	}
	fmt.Println(string(buf[:n]))
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, "udpecho:", message)
	os.Exit(1)
}
