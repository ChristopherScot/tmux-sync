// Command tmux-sync ports a live remote tmux session to your laptop and back.
//
// It captures the full remote session — tmux layout, scrollback, every running
// nvim's open files/splits, claude's conversation transcript, and the working
// files — moves it to a container on your laptop, lets you work offline, and
// reconciles the changes back when you return.
//
// See SPEC.md in this repo for the design.
package main

import (
	"flag"
	"fmt"
	"os"
)

// Set at build time by GoReleaser.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func main() {
	flag.Usage = usage
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "checkout":
		cmdCheckout(args[1:])
	case "checkin":
		cmdCheckin(args[1:])
	case "status":
		cmdStatus(args[1:])
	case "list":
		cmdList(args[1:])
	case "version", "--version", "-v":
		fmt.Printf("tmux-sync %s (commit %s, built %s)\n", version, commit, date)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "tmux-sync: unknown command %q\n\n", args[0])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `tmux-sync — port a live remote tmux session to your laptop and back

usage: tmux-sync <command> [args...]

Commands:
  checkout --from <endpoint> [--session <name>]   pull a session down + reconstruct locally
  checkin  --to   <endpoint> [--session <name>]   push it back + resume on the remote
  status                                          what's checked out where
  list     --from <endpoint>                      sessions available to check out
  version                                         print version + build info
  help                                            this message

See SPEC.md for the design.
`)
}

func notYet(cmd string) {
	fmt.Fprintf(os.Stderr, "tmux-sync %s: not yet implemented — see SPEC.md\n", cmd)
	os.Exit(1)
}

func cmdCheckout(args []string) { notYet("checkout") }
func cmdCheckin(args []string)  { notYet("checkin") }
func cmdStatus(args []string)   { notYet("status") }
func cmdList(args []string)     { notYet("list") }
