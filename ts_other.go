//go:build !linux

package main

import (
	"net"
	"time"
)

func enableKernelRxTimestamps(conn net.PacketConn) bool { return false }

func readStamped(conn net.PacketConn, buf, oob []byte) (int, int64, error) {
	n, _, err := conn.ReadFrom(buf)
	return n, time.Now().UnixNano(), err
}

func kernelTCPRTT(c net.Conn) (float64, bool) { return 0, false }
