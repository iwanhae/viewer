package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	"viewer/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.RunRecoBackfill(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
