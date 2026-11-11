package main

import (
	"flag"
	"path/filepath"
)

func flagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(nil)
	return flags
}

func resolveRepositoryPath(path string) (string, error) {
	return filepath.Abs(path)
}
