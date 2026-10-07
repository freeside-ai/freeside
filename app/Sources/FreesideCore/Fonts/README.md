# Bundled Faces

The §15 type: IBM Plex Sans (chrome), IBM Plex Mono (the evidence
register), and the serif for titles. All three are SIL OFL 1.1; the
license texts sit beside the files.

`FreesideSerif-Medium.ttf` and `FreesideSerif-Regular.ttf` are Source
Serif 4 (Adobe, release 4.005R) instanced from
`SourceSerif4Variable-Roman.ttf` at `wght=500` and `wght=400`, both at
`opsz=20` so the two weights are one design. Medium carries titles and
the ask; Regular carries the statement face. Adobe ships no static
Medium, and the OFL reserves the name "Source" for unmodified files, so
both instances carry the family name "Freeside Serif". Regenerate them
with `../../../scripts/instance-serif-font.sh`; a regenerated file
differs from the committed one only in its `head` modification time.

The Plex files are the unmodified `fonts/complete/ttf` files from the
`@ibm/plex-sans@1.1.0` and `@ibm/plex-mono@2.5.0` releases.
