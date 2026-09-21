package main

import (
	"context"
	"log"
	"os"

	"github.com/dreamtrans/backend/internal/deployment"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "deploy-control" {
		if err := deployment.Control(os.Args[2], os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "deploy-import" {
		if err := importDeploymentState(); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	app := newApplication(context.Background())
	defer app.Close()
	return app.run()
}
