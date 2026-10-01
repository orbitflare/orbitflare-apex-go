package main

import (
	"fmt"
	"os"

	apex "github.com/orbitflare/orbitflare-apex-go"
)

func main() {
	key := os.Getenv("APEX_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "APEX_API_KEY is required")
		os.Exit(2)
	}
	fmt.Println(apex.ClientPubkey(key))
}
