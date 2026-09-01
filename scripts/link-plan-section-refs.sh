#!/usr/bin/env bash
# link-plan-section-refs.sh — link the plan's section citations to anchors.
#
# Usage: link-plan-section-refs.sh [--check] [<markdown-file>]
#
# Finds every "Section N", "Section N.M", "Sections N and M", and "§N"
# citation in the file (docs/plan.md by default) and turns the cited
# number into a Markdown link to the GitHub anchor of the numbered
# heading it names ("## N. Title" or "### N.M Title"), so the citation is
# clickable on the rendered page. Only the number is wrapped, so no word
# changes and a citation that wraps across a line break links the same
# way as one that does not. Citations inside fenced code blocks and
# headings are left alone and counted, and so are citations inside an
# inline code span or the label of an existing Markdown link, which
# would otherwise gain a nested link. An already-linked citation is not
# rewritten, so a second run changes nothing, but a list whose first
# number is already linked still has its remaining raw numbers linked.
#
# Every link a run leaves in place is still checked against the headings
# it finds, so renaming a heading cannot leave a stale destination behind
# a passing --check. A citation whose number has no matching heading, and
# a link whose destination is not that section's current anchor, are both
# broken references; the script reports every one and exits without
# writing.
#
# Local example:
#   bash scripts/link-plan-section-refs.sh
#   bash scripts/link-plan-section-refs.sh --check
#
# Exit codes:
#   0  the file is (now) fully linked
#   1  --check: the file would change (unlinked citations remain)
#   2  usage error, unreadable file, or a broken section reference
set -euo pipefail

PROG=link-plan-section-refs

usage() {
  echo "usage: $PROG [--check] [<markdown-file>]" >&2
  exit 2
}

check=0
file=""
while [ $# -gt 0 ]; do
  case "$1" in
    --check) check=1 ;;
    -h | --help) usage ;;
    -*) usage ;;
    *)
      [ -z "$file" ] || usage
      file=$1
      ;;
  esac
  shift
done

if [ -z "$file" ]; then
  script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
  file=$(cd "$script_dir/.." && pwd)/docs/plan.md
fi
[ -r "$file" ] || {
  echo "$PROG: cannot read $file" >&2
  exit 2
}

perl - "$file" "$check" "$PROG" <<'PERL'
use strict;
use warnings;
use utf8;
binmode STDOUT, ':encoding(UTF-8)';
binmode STDERR, ':encoding(UTF-8)';

my ($path, $check, $prog) = @ARGV;

open my $in, '<:encoding(UTF-8)', $path or die "$prog: $path: $!\n";
my @lines = do { local $/; split /\n/, <$in>, -1 };
close $in;

# GitHub's heading-anchor derivation: lowercase, drop everything except
# letters, digits, spaces, hyphens, and underscores, then spaces become
# hyphens. Numbered headings in the plan are unique, so no "-1" suffix.
sub github_anchor {
    my ($heading) = @_;
    my $anchor = lc $heading;
    $anchor =~ s/[^\p{L}\p{N}\s_-]//g;
    $anchor =~ s/ /-/g;
    return $anchor;
}

# A code fence opens on three or more backticks or tildes and closes
# only on a line of at least as many of the same character with nothing
# else on it, so a longer fence can quote a shorter one and a tilde
# fence is not closed by backticks.
my $fence_open_re = qr/^\s{0,3}(`{3,}|~{3,})/;
sub fence_closes {
    my ($line, $open) = @_;
    my $char = substr $open, 0, 1;
    my $len = length $open;
    return $line =~ /^\s{0,3}\Q$char\E{$len,}\s*$/;
}

# Pass 1: numbered headings outside fenced code -> section number -> anchor.
my %anchor;
my $fence;
for my $line (@lines) {
    if (defined $fence) {
        $fence = undef if fence_closes($line, $fence);
        next;
    }
    if ($line =~ $fence_open_re) { $fence = $1; next; }
    next unless $line =~ /^##\s+(\d+)\.\s+\S/ || $line =~ /^###\s+(\d+\.\d+)\s+\S/;
    my $num = $1;
    (my $heading = $line) =~ s/^#+\s+//;
    $heading =~ s/\s+$//;
    die "$prog: duplicate numbered heading $num\n" if exists $anchor{$num};
    $anchor{$num} = github_anchor($heading);
}
die "$prog: no numbered headings found in $path\n" unless %anchor;

# A section number: digits with one optional ".digits" group, not glued
# to a following word or a deeper dotted level ("1B.1", "5.13.2").
my $num_re = qr/\d+(?:\.\d+)?(?![\w.]\d|\w)/;
# List separators after the first cited number: ", ", ", and ", " and ",
# " or ", and a hyphen or en-dash range.
my $sep_re = qr/,\s+(?:(?:and|or)\s+)?|\s+(?:and|or)\s+|\s*[–-]\s*/;
# Spans a citation may sit inside that must survive intact: an inline
# code span, and a Markdown link or image, whose label would otherwise
# gain a nested link that renders as literal brackets.
my $span_re = qr/`[^`\n]*`|!?\[[^\[\]]*\]\([^()]*\)/;
# A cited number in a list may already be linked, by an earlier run or a
# hand edit. The pass matches it as an item so it can walk past it to
# the raw numbers after it; a list edited from "Sections 5 and 6" to
# "Sections [5](#5-architecture) and 6, and 7" would otherwise stop
# matching and leave the new numbers unlinked behind a passing --check.
my $item_re = qr/\[§?$num_re\]\(#[^()]*\)|$num_re/;

my @broken;
my ($new_links, $skipped_code, $skipped_heading) = (0, 0, 0);
my ($segment_start, $segment);

# $offset is where the citation starts in the current segment; it only
# serves the line number in a broken-reference report.
sub segment_lineno {
    my ($text, $offset) = @_;
    return $segment_start + (() = substr($text, 0, $offset) =~ /\n/g);
}

sub link_number {
    my ($text, $num, $offset) = @_;
    if (!exists $anchor{$num}) {
        my $lineno = segment_lineno($segment, $offset);
        push @broken, "$path:$lineno: Section $num has no numbered heading";
        return $text;
    }
    $new_links++;
    return "[$text](#$anchor{$num})";
}

# An already-linked item passes through untouched; check_links is what
# verifies its destination.
sub link_item {
    my ($item, $offset) = @_;
    return $item if substr($item, 0, 1) eq '[';
    return link_number($item, $item, $offset);
}

sub link_list {
    my ($word, $space, $first, $rest) = @_;
    my $offset = $-[0];
    my $out = $word . $space . link_item($first, $offset);
    $out .= $1 . link_item($2, $offset) while $rest =~ /\G($sep_re)($item_re)/gc;
    return $out;
}

# Prose is linked one segment at a time, where a segment is the run of
# lines between fences and headings, so the whitespace inside a citation
# may span a line break ("Section\n5.9" links like "Section 5.9").
sub link_segment {
    my ($text) = @_;
    $segment = $text;
    $text =~ s{($span_re)|(\bSections?)(\s+)($item_re)((?:$sep_re$item_re)*)}
              {defined $1 ? $1 : link_list($2, $3, $4, $5)}ge;
    $segment = $text;
    $text =~ s{($span_re)|(?<!\[)§($num_re)}
              {defined $1 ? $1 : link_number("§$2", $2, $-[0])}ge;
    return $text;
}

# Both passes step over an existing section link rather than rewrite it,
# so its destination is verified here instead: a heading rename changes
# the anchor while the cited number stays the same, which would
# otherwise leave every link to that section pointing nowhere while
# --check reports the file fully linked.
sub check_links {
    my ($text) = @_;
    while ($text =~ /\[§?($num_re)\]\(#([^)]*)\)/g) {
        my ($num, $dest, $offset) = ($1, $2, $-[0]);
        next if exists $anchor{$num} && $anchor{$num} eq $dest;
        my $lineno = segment_lineno($text, $offset);
        push @broken, exists $anchor{$num}
            ? "$path:$lineno: Section $num links to #$dest, not #$anchor{$num}"
            : "$path:$lineno: Section $num has no numbered heading";
    }
}

sub count_citations {
    my ($text) = @_;
    return scalar(() = $text =~ /\bSections?\s+\d|§\d/g);
}

# Pass 2: link citations outside fenced code and headings.
$fence = undef;
my @out;
my @prose;
my $lineno = 0;
my $flush = sub {
    return unless @prose;
    my $linked = link_segment(join "\n", @prose);
    check_links($linked);
    push @out, $linked;
    @prose = ();
};
for my $line (@lines) {
    $lineno++;
    if (defined $fence) {
        $fence = undef if fence_closes($line, $fence);
        $skipped_code += count_citations($line) if defined $fence;
        push @out, $line;
        next;
    }
    if ($line =~ $fence_open_re) {
        $flush->();
        $fence = $1;
        push @out, $line;
        next;
    }
    # An ATX heading, not any line that opens with "#": the plan has
    # paragraph lines starting with an issue reference (#265), and a
    # citation on one of those still has to be linked.
    if ($line =~ /^#{1,6}(?:\s|$)/) {
        $flush->();
        $skipped_heading += count_citations($line);
        push @out, $line;
        next;
    }
    $segment_start = $lineno unless @prose;
    push @prose, $line;
}
$flush->();

if (@broken) {
    print STDERR "$_\n" for @broken;
    print STDERR "$prog: ", scalar(@broken), " broken section reference(s); nothing written\n";
    exit 2;
}

my $text = join "\n", @out;
my $summary = sprintf "%s: %d new link(s); skipped %d citation(s) in code blocks and %d in headings",
    $prog, $new_links, $skipped_code, $skipped_heading;

if ($new_links == 0) {
    print "$summary; $path is fully linked\n";
    exit 0;
}
if ($check) {
    print STDERR "$summary; $path is not fully linked (run without --check)\n";
    exit 1;
}
open my $outfh, '>:encoding(UTF-8)', $path or die "$prog: $path: $!\n";
print {$outfh} $text;
close $outfh;
print "$summary; wrote $path\n";
PERL
