// Command ws-mesh-demo is a runnable, no-install demo client for the crier
// relay + mesh endpoints. It has two subcommands:
//
//	subscribe -topic TOPIC [-once]   listen on ws://…/relay/subscribe/<topic>
//	peer      -agent AGENT_ID        join the P2P mesh as <agentID>
//
// It uses only the Go standard library plus github.com/gorilla/websocket
// (v1.5.3, already in go.mod) — zero external installs.
//
// Addresses CR-GAP-050.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"

	"github.com/gorilla/websocket"
)

const defaultBaseURL = "http://127.0.0.1:18767"

func usage() {
	fmt.Fprintf(os.Stderr, `ws-mesh-demo — crier relay + mesh demo client (CR-GAP-050)

Usage:
  ws-mesh-demo [-url BASE] subscribe -topic TOPIC [-once]
  ws-mesh-demo [-url BASE] peer -agent AGENT_ID

Commands:
  subscribe  connect to ws://…/relay/subscribe/<topic> and print events.
             Prints "SUBSCRIBED <topic>" after the upgrade, then one
             "EVENT <payload>" line per received event (payload is the raw
             event JSON the relay fans out). With -once, exits 0 after the
             first event.
  peer       connect to ws://…/mesh/connect/<agentID> and hold the
             connection open so the relay keeps the peer registered.
             Prints "PEER CONNECTED <agentID>" after the upgrade.

Flags:
  -url   server base URL (default %s)
`, defaultBaseURL)
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("ws-mesh-demo: ")

	urlFlag := flag.String("url", defaultBaseURL, "server base URL (http:// or https://)")
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() < 1 {
		usage()
		os.Exit(2)
	}

	// Derive the WebSocket base from the HTTP base.
	wsBase := strings.Replace(*urlFlag, "http://", "ws://", 1)
	wsBase = strings.Replace(wsBase, "https://", "wss://", 1)

	switch flag.Arg(0) {
	case "subscribe":
		os.Exit(cmdSubscribe(wsBase, flag.Args()[1:]))
	case "peer":
		os.Exit(cmdPeer(wsBase, flag.Args()[1:]))
	default:
		fmt.Fprintf(os.Stderr, "ws-mesh-demo: unknown subcommand %q\n", flag.Arg(0))
		usage()
		os.Exit(2)
	}
}

// cmdSubscribe implements the "subscribe" subcommand. It exits 0 after the
// first received event when -once is set, 1 on any connection error.
func cmdSubscribe(wsBase string, args []string) int {
	fs := flag.NewFlagSet("subscribe", flag.ExitOnError)
	topic := fs.String("topic", "", "topic to subscribe to (required)")
	once := fs.Bool("once", false, "exit 0 after the first received event")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: ws-mesh-demo subscribe -topic TOPIC [-once]\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	if *topic == "" {
		fmt.Fprintln(os.Stderr, "ws-mesh-demo subscribe: -topic is required")
		fs.Usage()
		return 2
	}

	u := fmt.Sprintf("%s/relay/subscribe/%s", wsBase, url.PathEscape(*topic))
	conn, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		log.Printf("subscribe: dial %s: %v", u, err)
		return 1
	}
	defer conn.Close()

	fmt.Printf("SUBSCRIBED %s\n", *topic)
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			log.Printf("subscribe: read: %v", err)
			return 1
		}
		fmt.Printf("EVENT %s\n", payload)
		if *once {
			return 0
		}
	}
}

// cmdPeer implements the "peer" subcommand. It holds the WebSocket open
// (discard-read loop keeps gorilla's default ping handler auto-ponging, so
// the relay's keepalive never drops the peer) and exits 0 on clean close.
func cmdPeer(wsBase string, args []string) int {
	fs := flag.NewFlagSet("peer", flag.ExitOnError)
	agent := fs.String("agent", "", "agent ID to register as (required)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: ws-mesh-demo peer -agent AGENT_ID\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	if *agent == "" {
		fmt.Fprintln(os.Stderr, "ws-mesh-demo peer: -agent is required")
		fs.Usage()
		return 2
	}

	u := fmt.Sprintf("%s/mesh/connect/%s", wsBase, url.PathEscape(*agent))
	conn, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		log.Printf("peer: dial %s: %v", u, err)
		return 1
	}
	defer conn.Close()

	fmt.Printf("PEER CONNECTED %s\n", *agent)
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			// Connection closed: the relay removed us from the mesh.
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return 0
			}
			log.Printf("peer: read: %v", err)
			return 1
		}
	}
}
