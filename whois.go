package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// The one exception to RDAP-only.
//
// CentralNic's RDAP (.xyz and the other TLDs it runs) answers 404 for names
// the registry holds back -- premium inventory, blocked words -- exactly as it
// does for names nobody has: same status, same body, byte for byte. Those
// names are in no zone and have no registrar, but they cannot be registered
// either, and computer.xyz, gold.xyz and friends were showing up as available.
//
// The registry's whois can tell the two apart. A free name gets "DOMAIN NOT
// FOUND"; a held one gets a stub carrying only the domain name, with no
// registry ID, registrar or dates. So a 404 from a CentralNic endpoint is
// confirmed over port 43 before it is believed. Only the 404s pay for this,
// and only on these endpoints.

// whoisServer returns the port-43 server that can confirm an RDAP 404 from
// base, or "" when that endpoint's 404s can be taken at face value. It is a
// variable so tests can point it at a local listener.
var whoisServer = func(base, domain string) string {
	if !strings.Contains(base, "centralnic.com") {
		return ""
	}
	dot := strings.LastIndexByte(domain, '.')
	if dot < 0 {
		return ""
	}
	return "whois.nic." + domain[dot+1:] + ":43"
}

// confirmUnregistered decides what an RDAP 404 means for domain: available,
// or held by the registry. An answer it does not recognize is an error, so it
// is retried and then recorded as a failure rather than guessed at.
func confirmUnregistered(base, domain string, timeout time.Duration) (Record, error) {
	server := whoisServer(base, domain)
	if server == "" {
		return Record{Domain: domain, Status: statusAvail}, nil
	}
	text, err := whoisQuery(server, domain, timeout)
	if err != nil {
		return Record{}, fmt.Errorf("whois %s: %w", domain, err)
	}
	status, err := parseCentralNicWhois(text, domain)
	if err != nil {
		return Record{}, fmt.Errorf("whois %s: %w", domain, err)
	}
	return Record{Domain: domain, Status: status}, nil
}

func whoisQuery(server, domain string, timeout time.Duration) (string, error) {
	conn, err := net.DialTimeout("tcp", server, timeout)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	if _, err := io.WriteString(conn, domain+"\r\n"); err != nil {
		return "", err
	}
	b, err := io.ReadAll(io.LimitReader(conn, 64<<10))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// parseCentralNicWhois reads a CentralNic whois answer for a name RDAP could
// not find.
func parseCentralNicWhois(text, domain string) (string, error) {
	var name, id bool
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.Contains(line, "DOMAIN NOT FOUND") {
			return statusAvail, nil
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Domain Name":
			name = strings.EqualFold(strings.TrimSpace(v), domain)
		case "Registry Domain ID", "Registrar", "Creation Date":
			id = true
		}
	}
	switch {
	case name && !id:
		return statusReserved, nil
	case name:
		// A full registration that RDAP did not know about: most likely a
		// name registered between the two queries. Not ours to call.
		return "", fmt.Errorf("registered in whois but not in RDAP")
	}
	return "", fmt.Errorf("unrecognized answer: %.80q", text)
}
