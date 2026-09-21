//go:build !linux

package main

import (
	"fmt"
	"net"
	"time"
)

func enableKernelRxTimestamps(conn net.PacketConn) bool { return false }

func readStamped(conn net.PacketConn, buf, oob []byte) (int, int64, error) {
	n, _, err := conn.ReadFrom(buf)
	return n, time.Now().UnixNano(), err
}

func kernelTCPRTT(c net.Conn) (float64, bool) { return 0, false }

func stampFromOOB(oob []byte) int64 { return time.Now().UnixNano() }

func setTTL(conn net.PacketConn, ttl int, v6 bool) error {
	return fmt.Errorf("traceroute is only supported on Linux")
}
