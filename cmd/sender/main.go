package main

import (
	"log"

	"screen-share/internal/sender"
)

func main() {
	if err := sender.Run(); err != nil {
		log.Fatal(err)
	}
}
