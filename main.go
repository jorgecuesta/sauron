package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"sauron/config"
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
	validate := flag.Bool("validate", false, "Check the configuration file and exit: 0 if it is valid, 1 if not")
	flag.Parse()

	// Print version if requested
	if *showVersion {
		fmt.Println("Sauron " + version)
		os.Exit(0)
	}

	if *validate {
		os.Exit(validateConfig(*configPath, os.Stdout, os.Stderr))
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

// validateConfig loads the file at path as the server would at startup and
// reports whether it is valid. It contacts no node and binds no port.
func validateConfig(path string, stdout, stderr io.Writer) int {
	cfg, err := config.LoadMultiChainFile(path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", path, err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "%s: valid: %d networks, %d internal nodes, %d externals\n",
		path, len(cfg.Networks), len(cfg.Internals), len(cfg.Externals))
	for _, w := range config.ConfigWarnings(cfg) {
		_, _ = fmt.Fprintf(stderr, "%s: warning: %s\n", path, w)
	}
	return 0
}
