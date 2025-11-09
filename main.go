package main

import (
	"flag"
	"fmt"
	"os"

	"sauron/server"
)

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
	version := flag.Bool("version", false, "Print version information")
	flag.Parse()

	// Print version if requested
	if *version {
		fmt.Println("Sauron v1.0.0")
		fmt.Println("The All-Seeing Oracle for Pocket Network")
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
