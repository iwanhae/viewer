package main

import (
	"context"
	"errors"
	"log"
	"net/http"

	"viewer/internal/app"
)

func main() {
	if err := app.Run(context.Background()); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
