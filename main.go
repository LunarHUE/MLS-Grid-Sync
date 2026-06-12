package main

import (
	_ "github.com/lib/pq"
	"github.com/LunarHUE/MLS-Grid-Sync/cmd"
)

func main() {
	cmd.Execute()
}
