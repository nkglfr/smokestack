package main

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Listen address of the web service. The IP and the port can be set
// separately, which avoids the brackets IPv6 needs in "ip:port" strings.
// Highest priority first:
//
//	flags        -ip 0.0.0.0 -port 8080   (or -listen 0.0.0.0:8080)
//	environment  SMOKESTACK_LISTEN_IP, SMOKESTACK_LISTEN_PORT
//	             (set in /etc/smokestack/smokestack.env by the installer)
//	config.json  "listen_ip", "listen_port" (or the older "listen")
//	default      127.0.0.1:8080
//
// An empty IP, "*", "0.0.0.0" or "::" means all interfaces.

const (
	defaultListenIP   = "127.0.0.1"
	defaultListenPort = 8080
)

type listenSource struct {
	flagListen, flagIP, flagPort string
	getenv                       func(string) string
}

func normalizeListenIP(ip string) (string, error) {
	ip = strings.Trim(strings.TrimSpace(ip), "[]")
	switch ip {
	case "", "*":
		return "", nil
	}
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("invalid listen IP %q (expected an IPv4 or IPv6 address, or * for all interfaces)", ip)
	}
	return ip, nil
}

func parseListenPort(p string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(p))
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("invalid listen port %q (expected 1 to 65535)", p)
	}
	return n, nil
}

// resolveListen returns the address to listen on, or an error that names
// the faulty setting.
func resolveListen(cfg Config, src listenSource) (string, error) {
	ip, port := defaultListenIP, defaultListenPort

	// config.json: the older combined "listen", then the separate fields.
	if cfg.Listen != "" {
		h, p, err := net.SplitHostPort(cfg.Listen)
		if err != nil {
			return "", fmt.Errorf("config.json \"listen\": %v", err)
		}
		if ip, err = normalizeListenIP(h); err != nil {
			return "", fmt.Errorf("config.json \"listen\": %v", err)
		}
		if port, err = parseListenPort(p); err != nil {
			return "", fmt.Errorf("config.json \"listen\": %v", err)
		}
	}
	var err error
	if cfg.ListenIP != "" {
		if ip, err = normalizeListenIP(cfg.ListenIP); err != nil {
			return "", fmt.Errorf("config.json \"listen_ip\": %v", err)
		}
	}
	if cfg.ListenPort != 0 {
		if port, err = parseListenPort(strconv.Itoa(cfg.ListenPort)); err != nil {
			return "", fmt.Errorf("config.json \"listen_port\": %v", err)
		}
	}

	// Environment.
	if v := src.getenv("SMOKESTACK_LISTEN_IP"); v != "" {
		if ip, err = normalizeListenIP(v); err != nil {
			return "", fmt.Errorf("SMOKESTACK_LISTEN_IP: %v", err)
		}
	}
	if v := src.getenv("SMOKESTACK_LISTEN_PORT"); v != "" {
		if port, err = parseListenPort(v); err != nil {
			return "", fmt.Errorf("SMOKESTACK_LISTEN_PORT: %v", err)
		}
	}

	// Flags.
	if src.flagListen != "" {
		h, p, err := net.SplitHostPort(src.flagListen)
		if err != nil {
			return "", fmt.Errorf("-listen: %v", err)
		}
		if ip, err = normalizeListenIP(h); err != nil {
			return "", fmt.Errorf("-listen: %v", err)
		}
		if port, err = parseListenPort(p); err != nil {
			return "", fmt.Errorf("-listen: %v", err)
		}
	}
	if src.flagIP != "" {
		if ip, err = normalizeListenIP(src.flagIP); err != nil {
			return "", fmt.Errorf("-ip: %v", err)
		}
	}
	if src.flagPort != "" {
		if port, err = parseListenPort(src.flagPort); err != nil {
			return "", fmt.Errorf("-port: %v", err)
		}
	}
	return net.JoinHostPort(ip, strconv.Itoa(port)), nil
}

// listenNote explains, in the log, what the address means for visitors.
func listenNote(addr string) string {
	h, _, _ := net.SplitHostPort(addr)
	ip := net.ParseIP(h)
	switch {
	case h == "" || (ip != nil && ip.IsUnspecified()):
		return "all interfaces: reachable from the network, put a TLS reverse proxy in front before exposing it"
	case ip != nil && ip.IsLoopback():
		return "this machine only: reach it through a reverse proxy, or set SMOKESTACK_LISTEN_IP"
	}
	return "this address only"
}
