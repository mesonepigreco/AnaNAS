// ananas-route-probe is an explicit, fixed-address network-namespace test. It
// exercises the same socket policy as the transfer client/helper, without user
// files, TLS, production endpoints or daemon installation.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"nas-sync/internal/socketpolicy"
)

func main() {
	if err := run(); err != nil {
		json.NewEncoder(os.Stdout).Encode(map[string]any{"event": "failed", "error": err.Error()})
		os.Exit(1)
	}
}

func run() error {
	host := flag.String("host-netns", "", "original host network namespace identity")
	serve := flag.Bool("serve", false, "serve the fixed isolated echo fixture")
	plain := flag.Bool("plain-control", false, "positive control allowing gateways, isolated namespace only")
	hold := flag.Bool("hold", false, "wait for 'again' on stdin between two exchanges")
	port := flag.Int("port", 8742, "fixture port: 8742 or 8743")
	sourcePort := flag.Int("source-port", 30001, "fixture source port 30001..30100")
	flag.Parse()
	current, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return err
	}
	initNS, err := os.Readlink("/proc/1/ns/net")
	if err != nil {
		return err
	}
	if flag.NArg() != 0 || !regexp.MustCompile(`^net:\[[0-9]+\]$`).MatchString(*host) || current == *host || current == initNS || os.Geteuid() != 0 || (*port != 8742 && *port != 8743) || *sourcePort < 30001 || *sourcePort > 30100 {
		return fmt.Errorf("fixed fixture in a separate privileged network namespace required")
	}
	device := "lan0"
	if *serve {
		device = "nas0"
	}
	control := func(_, _ string, raw syscall.RawConn) error {
		var policyErr error
		err := raw.Control(func(fd uintptr) {
			if *plain {
				policyErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, device)
			} else {
				policyErr = socketpolicy.BindIPv4(int(fd), device)
			}
		})
		return errors.Join(err, policyErr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	limit := 10 * time.Second
	if *serve {
		limit = 75 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	endpoint := fmt.Sprintf("10.88.0.30:%d", *port)
	if *serve {
		lc := net.ListenConfig{Control: control, KeepAlive: -1}
		listener, err := lc.Listen(ctx, "tcp4", endpoint)
		if err != nil {
			return err
		}
		defer listener.Close()
		closeOnCancel := context.AfterFunc(ctx, func() { listener.Close() })
		defer closeOnCancel()
		json.NewEncoder(os.Stdout).Encode(map[string]any{"event": "serving", "namespace": current, "plain": *plain})
		for n := 0; n < 32; n++ {
			conn, err := listener.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			if !*plain {
				if err := socketpolicy.VerifyConn(conn, device); err != nil {
					conn.Close()
					return err
				}
			}
			for count := 0; count < 2; count++ {
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				var data [64]byte
				if _, err := io.ReadFull(conn, data[:]); err != nil {
					break
				}
				if _, err := conn.Write(data[:]); err != nil {
					break
				}
			}
			conn.Close()
		}
		return nil
	}
	dialer := net.Dialer{Timeout: 2 * time.Second, KeepAlive: -1, Control: control, LocalAddr: &net.TCPAddr{IP: net.ParseIP("10.88.0.2"), Port: *sourcePort}}
	conn, err := dialer.DialContext(ctx, "tcp4", endpoint)
	if err != nil {
		return err
	}
	defer conn.Close()
	if !*plain {
		if err := socketpolicy.VerifyConn(conn, device); err != nil {
			return err
		}
	}
	exchange := func(number int) error {
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		var data, got [64]byte
		copy(data[:], fmt.Sprintf("anaNAS isolated route fixture exchange %d", number))
		if _, err := conn.Write(data[:]); err != nil {
			return err
		}
		if _, err := io.ReadFull(conn, got[:]); err != nil {
			return err
		}
		if data != got {
			return fmt.Errorf("fixture echo differs")
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"event": "exchange", "number": number, "plain": *plain, "namespace": current})
	}
	if err := exchange(1); err != nil {
		return err
	}
	if *hold {
		closeInput := context.AfterFunc(ctx, func() { os.Stdin.Close() })
		defer closeInput()
		var command [6]byte
		if _, err := io.ReadFull(os.Stdin, command[:]); err != nil || string(command[:]) != "again\n" {
			return fmt.Errorf("bounded continuation command required")
		}
		return exchange(2)
	}
	return nil
}
