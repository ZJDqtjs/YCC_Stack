package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// nodeConfig lets the downloaded exe run by double-click: the broker puts this
// file next to the worker, so the machine joins without a bat that rebuilds a
// long flag list. Explicit command-line flags always win, which keeps the bat
// and manual runs working as the fallback path.
type nodeConfig struct {
	Server        string   `json:"server"`
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Price         *float64 `json:"price"`
	Slots         *int     `json:"slots"`
	Launch        string   `json:"launch"`
	Token         string   `json:"token"`
	Work          string   `json:"work"`
	AllowBuild    *bool    `json:"allow_build"`
	AutoProvision *bool    `json:"auto_provision"`
}

// loadNodeConfig looks next to the executable first: that is where the bundle
// unpacks, and it is the only location a double-clicked exe can rely on.
func loadNodeConfig(explicit, work string) (*nodeConfig, string) {
	var paths []string
	if explicit != "" {
		paths = append(paths, explicit)
	}
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), "starstack-node.json"))
	}
	if work != "" {
		paths = append(paths, filepath.Join(work, "starstack-node.json"))
	}
	paths = append(paths, "starstack-node.json")
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var c nodeConfig
		if err := json.Unmarshal(b, &c); err != nil {
			continue
		}
		return &c, p
	}
	return nil, ""
}
