//go:build race

package main

import "time"

func raceBuildFlags() []string { return []string{"-race"} }

func readinessDeadline() time.Duration { return 30 * time.Second }
