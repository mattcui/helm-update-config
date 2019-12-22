package main

import (
	"os"
)

func main() {
	cmd := newUpdatecfgCmd()
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
