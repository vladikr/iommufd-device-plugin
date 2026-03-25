package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/vladikr/iommufd-device-plugin/pkg/plugin"
)

func main() {
	logLevel := flag.String("log-level", "info", "Log level (debug, info, warn, error)")
	socketDir := flag.String("socket-dir", "/var/run/kubevirt/fd-sockets", "Directory for IOMMUFD FD-passing sockets")
	flag.Parse()

	log.Printf("Starting IOMMUFD device plugin (log-level=%s, socket-dir=%s)", *logLevel, *socketDir)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	stopCh := make(chan struct{})
	srv := plugin.NewIOMMUFDDevicePlugin(*socketDir)

	go func() {
		if err := srv.Start(stopCh); err != nil {
			log.Fatalf("device plugin exited with error: %v", err)
		}
	}()

	sig := <-sigCh
	log.Printf("Received signal %v, shutting down", sig)
	close(stopCh)
}
