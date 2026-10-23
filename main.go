package main

import (
	"flag"
	"fmt"
	"os"

	"sauron/server"
)

// version is set at build time with -ldflags "-X main.version=<tag>".
var version = "dev"

const banner = `
███████╗ █████╗ ██╗   ██╗██████╗  ██████╗ ███╗   ██╗
██╔════╝██╔══██╗██║   ██║██╔══██╗██╔═══██╗████╗  ██║
███████╗███████║██║   ██║██████╔╝██║   ██║██╔██╗ ██║
╚════██║██╔══██║██║   ██║██╔══██╗██║   ██║██║╚██╗██║
███████║██║  ██║╚██████╔╝██║  ██║╚██████╔╝██║ ╚████║
╚══════╝╚═╝  ╚═╝ ╚═════╝ ╚═╝  ╚═╝ ╚═════╝ ╚═╝  ╚═══╝

The All-Seeing Oracle for Pocket Network
"One Sauron to watch them, One Sauron to link,
 One Sauron to route them all, and in the metrics bind them"`

func main() {
	// Parse flags
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	showVersion := flag.Bool("version", false, "Print version information")
	flag.Parse()

	// Print version if requested
	if *showVersion {
		fmt.Println("Sauron " + version)
		os.Exit(0)
	}

	// Print banner
	fmt.Println(banner)

	// Create and start server
	srv, err := server.New(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Fatal error: %v\n", err)
		os.Exit(1)
	}

	if err := srv.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start server: %v\n", err)
		os.Exit(1)
	}

	// Wait for shutdown signal
	srv.WaitForShutdown()
}
