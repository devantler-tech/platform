package main

import (
	"log"
	"time"

	"github.com/devantler-tech/platform/pkg/installer"
	"github.com/devantler-tech/platform/pkg/installer/flux"
	"github.com/devantler-tech/platform/pkg/installer/kubectl"
)

func main() {
	kubeconfig := "~/.kube/config"
	context := ""
	timeout := 5 * time.Minute

	// Create installers
	var fluxInstaller installer.Installer = flux.New(kubeconfig, context, timeout)
	var kubectlInstaller installer.Installer = kubectl.New(kubeconfig, context, timeout)

	// Use the installers
	installers := []installer.Installer{
		kubectlInstaller, // kubectl installer should typically go first
		fluxInstaller,
	}

	for i, inst := range installers {
		log.Printf("Installing component %d...", i+1)
		if err := inst.Install(); err != nil {
			log.Fatalf("Failed to install component %d: %v", i+1, err)
		}
		log.Printf("Component %d installed successfully", i+1)
	}

	log.Println("All components installed successfully!")
}