// SPDX-License-Identifier: GPL-3.0-or-later

package channel

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/bmmmm/stattii/internal/core"
)

// fakeSMTPServer speaks just enough SMTP to accept one message: banner,
// EHLO (no STARTTLS/AUTH extensions offered), MAIL/RCPT/DATA, QUIT. It
// records the raw DATA payload for the caller to inspect.
func fakeSMTPServer(t *testing.T) (addr string, captured chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	captured = make(chan string, 1)
	go func() {
		defer ln.Close()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		fmt.Fprint(conn, "220 test.local ESMTP ready\r\n")
		readLine := func() string {
			line, _ := r.ReadString('\n')
			return strings.TrimRight(line, "\r\n")
		}
		readLine() // EHLO
		fmt.Fprint(conn, "250 test.local\r\n")
		readLine() // MAIL FROM
		fmt.Fprint(conn, "250 OK\r\n")
		readLine() // RCPT TO
		fmt.Fprint(conn, "250 OK\r\n")
		readLine() // DATA
		fmt.Fprint(conn, "354 End data with <CR><LF>.<CR><LF>\r\n")
		var body strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if strings.TrimRight(line, "\r\n") == "." {
				break
			}
			body.WriteString(line)
		}
		captured <- body.String()
		fmt.Fprint(conn, "250 OK: queued\r\n")
		readLine() // QUIT
		fmt.Fprint(conn, "221 Bye\r\n")
	}()
	return ln.Addr().String(), captured
}

func TestEmailSendIncludesDateAndMessageID(t *testing.T) {
	addr, captured := fakeSMTPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}

	e := &Email{Host: host, Port: port, From: "sender@example.com"}
	if err := e.Send("recipient@example.com", core.Message{Subject: "Hi", Body: "hello"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case msg := <-captured:
		if !strings.Contains(msg, "\r\nDate: ") {
			t.Errorf("message missing Date header:\n%s", msg)
		}
		if !strings.Contains(msg, "\r\nMessage-ID: <") {
			t.Errorf("message missing Message-ID header:\n%s", msg)
		}
		if !strings.Contains(msg, "hello") {
			t.Errorf("message missing body:\n%s", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server never captured a message")
	}
}

// TestEmailSendTimesOutOnStall proves the fix for the original bug: a peer
// that accepts the TCP connection and then stalls (no banner, ever) must
// not block Send forever. The Email uses tiny unexported timeouts so the
// test itself stays fast.
func TestEmailSendTimesOutOnStall(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Accept the connection and never write anything back — the peer
		// that motivated this fix.
		<-t.Context().Done()
		conn.Close()
	}()

	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	e := &Email{
		Host:           host,
		Port:           port,
		From:           "sender@example.com",
		dialTimeout:    2 * time.Second,
		sessionTimeout: 200 * time.Millisecond,
	}

	done := make(chan error, 1)
	go func() {
		done <- e.Send("recipient@example.com", core.Message{Subject: "Hi", Body: "hello"})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Send against a stalling peer returned nil error; expected a timeout")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Send hung past the session timeout — the stall is not bounded")
	}
}
