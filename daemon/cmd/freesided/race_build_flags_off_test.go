//go:build !race

package main

import "time"

func raceBuildFlags() []string { return nil }

func readinessDeadline() time.Duration { return 5 * time.Second }
