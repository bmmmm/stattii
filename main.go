// SPDX-License-Identifier: GPL-3.0-or-later

// stattii — a secure attestation layer over an event calendar: responsible
// people confirm or cancel events via tokenized links, cancellations are
// propagated outward with delivery proof, automations hang off signed
// webhooks and an ICS feed.
package main

import (
	"fmt"
	"log"
	"os"
	"runtime/debug"
)

var version = "dev"

func versionString() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}

func usage() {
	fmt.Fprint(os.Stderr, `stattii — event confirmation/cancellation layer

Server:
  stattii serve [--listen :8788] [--data ./data] [--base-url URL] ...

Client (uses STATTII_URL and STATTII_TOKEN — point them at the ADMIN listener):
  stattii overview [--all]
  stattii calendar fetch
  stattii series-assign <source-uid> <person-id> [role]
  stattii event    list | create | show | confirm | cancel | reinstate | move | links | revoke-links | responses | propagation | invite | guests
  stattii person   list | add | test | rotate-portal
  stattii assign   <event-id> <person-id> [role]
  stattii broadcast list | add | rm
  stattii webhook  list | add | rm
  stattii proposal list | accept | reject
  stattii outbox   list [--pending] | retry <id>
  stattii audit    [--limit N]
  stattii tick
  stattii version

Run 'stattii serve --help' for server flags; leaf commands take the flags
shown next to them (e.g. 'stattii event move <id> --at ...').
`)
}

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(versionString())
	case "help", "--help", "-h":
		usage()
	default:
		if err := cmdClient(os.Args[1:]); err != nil {
			log.Fatalf("stattii: %v", err)
		}
	}
}
