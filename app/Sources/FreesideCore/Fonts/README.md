# Bundled Faces

The §15 type: IBM Plex Sans (chrome), IBM Plex Mono (the evidence
register), and the serif for titles. All three are SIL OFL 1.1; the
license texts sit beside the files.

`FreesideSerif-Medium.ttf` is Source Serif 4 (Adobe, release 4.005R)
instanced at `wght=500, opsz=20` from `SourceSerif4Variable-Roman.ttf`.
Adobe ships no static Medium, and the OFL reserves the name "Source"
for unmodified files, so the instance carries the family name "Freeside
Serif". Regenerate it with `../../../scripts/instance-serif-font.sh`.

The Plex files are the unmodified `fonts/complete/ttf` files from the
`@ibm/plex-sans@1.1.0` and `@ibm/plex-mono@2.5.0` releases.
