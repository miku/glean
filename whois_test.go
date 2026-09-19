package main

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Trimmed from live whois.nic.xyz answers.
const (
	whoisHeld = "Domain Name: COMPUTER.XYZ\r\n" +
		"URL of the ICANN Whois Inaccuracy Complaint Form: https://www.icann.org/wicf/\r\n" +
		">>> Last update of WHOIS database: 2026-09-19T13:32:28.0Z <<<\r\n"
	whoisFree = "The queried object does not exist: DOMAIN NOT FOUND\r\n\r\n" +
		">>> Last update of WHOIS database: 2026-09-19T13:32:28.0Z <<<\r\n"
	whoisRegistered = "Domain Name: ABC.XYZ\r\n" +
		"Registry Domain ID: D2192285-CNIC\r\n" +
		"Creation Date: 2014-03-20T12:59:17.0Z\r\n" +
		"Registrar: MarkMonitor Inc.\r\n"
)

func TestParseCentralNicWhois(t *testing.T) {
	cases := []struct {
		name, text, domain, want string
		wantErr                  bool
	}{
		{"held by registry", whoisHeld, "computer.xyz", statusReserved, false},
		{"free", whoisFree, "doctor.xyz", statusAvail, false},
		{"registered after all", whoisRegistered, "abc.xyz", "", true},
		{"stub for another name", whoisHeld, "box.xyz", "", true},
		{"rate limited", "Rate limit exceeded\r\n", "box.xyz", "", true},
		{"empty", "", "box.xyz", "", true},
	}
	for _, c := range cases {
		got, err := parseCentralNicWhois(c.text, c.domain)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("%s: got (%q, %v), want %q (err %v)", c.name, got, err, c.want, c.wantErr)
		}
	}
}

func TestWhoisServer(t *testing.T) {
	if got := whoisServer("https://rdap.centralnic.com/xyz/", "box.xyz"); got != "whois.nic.xyz:43" {
		t.Errorf("centralnic: got %q", got)
	}
	if got := whoisServer("https://rdap.verisign.com/com/v1/", "box.com"); got != "" {
		t.Errorf("verisign: got %q, want no whois", got)
	}
}

// TestLookupHeldName runs the whole path: RDAP 404, then whois tells the held
// name from the free one.
func TestLookupHeldName(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			q, _ := bufio.NewReader(conn).ReadString('\n')
			if strings.TrimSpace(q) == "computer.xyz" {
				conn.Write([]byte(whoisHeld))
			} else {
				conn.Write([]byte(whoisFree))
			}
			conn.Close()
		}
	}()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"errorCode":404,"title":"Object not found"}`))
	}))
	defer srv.Close()

	old := whoisServer
	whoisServer = func(base, domain string) string { return ln.Addr().String() }
	defer func() { whoisServer = old }()

	for domain, want := range map[string]string{
		"computer.xyz": statusReserved,
		"doctor.xyz":   statusAvail,
	} {
		got, err := lookup(srv.URL, domain, 5*time.Second)
		if err != nil {
			t.Fatalf("%s: %v", domain, err)
		}
		if got.Status != want {
			t.Errorf("%s: status = %q, want %q", domain, got.Status, want)
		}
	}
}
