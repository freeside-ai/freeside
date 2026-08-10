// Package atomicfile owns the daemon's durable file-publication primitives.
// A committed replacement is staged beside its target, synced and closed,
// atomically renamed into place, then followed by a parent-directory sync.
// RenameNoReplace supplies the same publication boundary without clobbering
// an existing target; its caller remains responsible for the directory sync.
package atomicfile
