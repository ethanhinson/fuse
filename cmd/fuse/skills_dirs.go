package main

import (
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/skills"
)

// skillDirsFromConfig resolves the filesystem directories skills load from: config
// skills.dirs, falling back to skills.DefaultDirs().
func skillDirsFromConfig(cfg config.Config) []string {
	if len(cfg.Skills.Dirs) > 0 {
		return cfg.Skills.Dirs
	}
	return skills.DefaultDirs()
}
