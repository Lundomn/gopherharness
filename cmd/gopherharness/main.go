package main

import (
	"github.com/Lundomn/gopherharness/internal/app"
	"os"
)

func main() { os.Exit(app.Run(os.Args[1:], app.Options{})) }
