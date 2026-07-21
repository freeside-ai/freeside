//go:build race

package main

func raceBuildFlags() []string { return []string{"-race"} }
