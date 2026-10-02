// Package agenttree reads and renders the admitted-agent tree (plan §5.4,
// §5.8): the operator-authored agents, fragments, lineup, name-to-digest map,
// and attended marks. policy/README.md fixes the layout and the file formats.
//
// The daemon reads one revision of the tree from an exact commit (ReadCommit,
// Parse) and never writes it. The render side (Render, Patch) exists so
// `freesided auth adopt` can print a patch that a human reviews and commits.
package agenttree
