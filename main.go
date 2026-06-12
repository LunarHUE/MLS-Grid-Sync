package main

import (
	_ "github.com/lib/pq"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/cmd"
)

func main() {
	cmd.Execute()
}
