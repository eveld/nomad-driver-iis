package main

import (
	log "github.com/hashicorp/go-hclog"

	"github.com/hashicorp/nomad/plugins"

	"github.com/eveld/nomad-driver-iis/pkg/driver"
)

func main() {
	// Serve the plugin
	plugins.Serve(factory)
}

// factory returns a new instance of a nomad driver plugin
func factory(log log.Logger) interface{} {
	return driver.NewIISDriver(log)
	// return singularity.NewSingularityDriver(log)
}

func init() {
	// Initialize user agent strings
	// useragent.InitValue("singularity", "3.1.1")
}
