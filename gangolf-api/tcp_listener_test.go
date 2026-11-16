package main

import (
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestListenTCPExplicitIPv4WildcardIsIPv4Only(t *testing.T) {
	listener, err := listenTCP("0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	raw, err := listener.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	family := ""
	var socketErr error
	if err = raw.Control(func(fd uintptr) {
		address, err := syscall.Getsockname(int(fd))
		socketErr = err
		switch address.(type) {
		case *syscall.SockaddrInet4:
			family = "IPv4"
		case *syscall.SockaddrInet6:
			family = "IPv6"
		}
	}); err != nil || socketErr != nil {
		t.Fatalf("socket inspection failed: %v %v", err, socketErr)
	}
	if family != "IPv4" {
		t.Fatalf("explicit IPv4 wildcard opened %s socket: %s", family, listener.Addr())
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", port), time.Second)
	if err != nil {
		t.Fatal("IPv4 loopback cannot reach IPv4 wildcard")
	}
	conn.Close()
	if conn, err = net.DialTimeout("tcp6", net.JoinHostPort("::1", port), 100*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("explicit IPv4 wildcard accepted IPv6")
	}
}

func TestListenTCPPreservesLiteralIPv6AndHostname(t *testing.T) {
	for _, address := range []string{"127.0.0.1:0", "[::1]:0", "localhost:0"} {
		t.Run(address, func(t *testing.T) {
			listener, err := listenTCP(address)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			if address == "[::1]:0" && listener.Addr().(*net.TCPAddr).IP.To4() != nil {
				t.Fatal("IPv6 literal became IPv4")
			}
			conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			conn.Close()
		})
	}
}
