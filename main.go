package main

import (
	"os"

	"dos-shell/shell"
)

func main() {
	s := shell.New()
	s.Run(os.Args[1:])
}
