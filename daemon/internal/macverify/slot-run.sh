#!/bin/sh
# slot-run: the macOS verification executor's one root door (#2877).
#
# freesided runs as the operator's LaunchAgent and has no root, but running a
# recipe command as a slot account needs three root operations: preparing the
# slot's job directory and home, launching in the slot's own launchd context
# (launchctl asuser), and killing everything the slot owns. The daemon reaches
# them only through this script, invoked as `sudo -n <root>/bin/slot-run`
# under a sudoers rule naming that one path. The daemon embeds these bytes and
# refuses a job when the installed copy differs.
#
# Operations (every slot argument must be listed in slots.conf beside this
# script, with its reserved ports in ports.conf; a job id is 32 lowercase hex
# characters):
#
#   prepare <slot> <job-id> <workdir> [swift=<key>] [go=<key>]
#                                       sandbox profile on stdin
#   run     <slot> <job-id> HOME=… TMPDIR=… PATH=… LC_ALL=C
#                 FREESIDE_VERIFY_PORTS=… [GO…=…]
#                 JOB_DIR=… SLOT_HOME=… SLOT_TMP=… SLOT_CACHE=…
#                 PORT_1=… PORT_2=… PORT_3=… PORT_4=… -- <argv>…
#   reap    <slot>
#   cleanup <slot> <job-id>
#
# prepare and reap print key=value lines the daemon checks; run replaces this
# process with the command and exits with its status. A Swift cache key
# (64 lowercase hex characters) names an entry under <root>/swift-cache
# whose seed prepare copies into the job directory and the slot's home
# (#2880); the mirrors the seed points at stay in the entry, read in place.
# A Go cache key names an entry under <root>/go-cache whose module cache the
# command reads in place, with the pinned toolchain beside it (#3155).
# prepare records the key root-owned beside the job's profile, and run
# derives the command's Go environment from that record alone, so the
# daemon's argument cannot point the command at another cache.
#
# ports.conf names each slot's four reserved ports, one line per slot:
# `<slot> <p1> <p2> <p3> <p4>` (#3155). The sandbox profile admits only
# those ports, through its PORT_1 to PORT_4 parameters, and the command
# finds them in FREESIDE_VERIFY_PORTS, comma-separated. prepare reports them
# so the daemon can check them against its configuration.
#
# Trust: the executor root is the parent of this script's directory, never an
# argument, and every directory from here to / must be root-owned and not
# group- or world-writable. That check is what stops a hard link to this file
# from elsewhere (which sudo would match by inode) from supplying its own
# slots.conf. Destructive work runs only while the slot owns no process.
#
# Exit status: 0 on success. Any refusal or failure before run hands over to
# the command exits REFUSED, so the daemon can tell a refused run from a
# command; a command that itself exits REFUSED reads as a refused run.

set -eu
PATH=/usr/bin:/bin:/usr/sbin:/sbin
# Bracket ranges in patterns follow the locale's collation; pin it.
LC_ALL=C
export PATH LC_ALL
IFS='
'
umask 022

REFUSED=120
# COMMAND_PATH is the only PATH a recipe command sees. The daemon's copy of
# this constant must match (TestHelperScriptConstants).
COMMAND_PATH=/usr/bin:/bin:/usr/sbin:/sbin
# GO_VERSION is the pinned Go toolchain's version; the daemon's copy must
# match (TestHelperScriptConstants).
GO_VERSION=1.27.4
MAX_PROFILE_BYTES=262144

# shellcheck disable=SC2154 # status is assigned inside the trap itself
trap 'status=$?; [ "$status" -eq 0 ] || exit "$REFUSED"' EXIT

refuse() {
	printf 'slot-run: %s\n' "$*" >&2
	exit "$REFUSED"
}

# root_owned refuses a path that is not owned by root or that group or others
# may write. stat without -L inspects a symlink itself, not its target.
root_owned() {
	meta=$(stat -f '%u %Lp' "$1") || refuse "cannot inspect $1"
	owner=${meta% *}
	perm=${meta#* }
	[ "$owner" = 0 ] || refuse "$1 is not owned by root"
	[ $((0$perm & 022)) -eq 0 ] || refuse "$1 is writable by group or others"
}

self=$0
case $self in
/*) ;;
*) refuse "invoke by absolute path" ;;
esac
bindir=${self%/*}
[ "${bindir##*/}" = bin ] || refuse "helper must live in <executor-root>/bin"
canonical=$(cd -P "$bindir" && pwd -P) || refuse "cannot resolve $bindir"
[ "$canonical" = "$bindir" ] || refuse "$bindir is not a canonical path"
root=${bindir%/*}
[ -n "$root" ] || refuse "executor root may not be /"
case $root in
*[!A-Za-z0-9/._-]*) refuse "executor root $root has characters outside [A-Za-z0-9/._-]" ;;
esac
dir=$bindir
while :; do
	root_owned "$dir"
	[ "$dir" = / ] && break
	dir=${dir%/*}
	[ -n "$dir" ] || dir=/
done
[ -f "$self" ] && [ ! -L "$self" ] || refuse "$self is not a regular file"
root_owned "$self"
conf=$bindir/slots.conf
portsconf=$bindir/ports.conf
for file in "$conf" "$portsconf"; do
	[ -f "$file" ] && [ ! -L "$file" ] || refuse "$file is not a regular file"
	root_owned "$file"
done
for dir in "$root/jobs" "$root/homes" "$root/template" "$root/template/home"; do
	[ -d "$dir" ] && [ ! -L "$dir" ] || refuse "$dir is not a directory"
	root_owned "$dir"
done
caller=${SUDO_UID:-}
case $caller in
'' | *[!0-9]*) refuse "run through sudo" ;;
esac
[ "$caller" -ne 0 ] || refuse "the caller may not be root"

# select_slot sets slot, uid, home, and jobs for a slot named in slots.conf.
select_slot() {
	slot=$1
	case $slot in
	[a-z_]*) ;;
	*) refuse "malformed slot name" ;;
	esac
	case $slot in
	*[!a-z0-9_-]*) refuse "malformed slot name" ;;
	esac
	[ "${#slot}" -le 32 ] || refuse "malformed slot name"
	listed=no
	while IFS= read -r line || [ -n "$line" ]; do
		if [ "$line" = "$slot" ]; then
			listed=yes
			break
		fi
	done <"$conf"
	[ "$listed" = yes ] || refuse "slot $slot is not in $conf"
	slot_ports=
	while IFS= read -r line || [ -n "$line" ]; do
		case $line in
		"$slot "*)
			[ -z "$slot_ports" ] || refuse "$portsconf lists $slot twice"
			slot_ports=${line#"$slot "}
			;;
		esac
	done <"$portsconf"
	[ -n "$slot_ports" ] || refuse "slot $slot has no ports in $portsconf"
	select_ports "$slot_ports"
	uid=$(id -u "$slot" 2>/dev/null) || refuse "no account named $slot"
	case $uid in
	'' | *[!0-9]*) refuse "account $slot has no numeric uid" ;;
	esac
	[ "$uid" -ne 0 ] || refuse "slot $slot is root"
	[ "$uid" -ne "$caller" ] || refuse "slot $slot is the caller"
	groups=$(id -Gn "$slot") || refuse "cannot read the groups of $slot"
	case " $groups " in
	*" admin "*) refuse "slot $slot is an administrator" ;;
	esac
	home=$root/homes/$slot
	jobs=$root/jobs/$slot
	# Toolchains find the home through the directory service, not $HOME
	# (SwiftPM does), so the account's own home must be the one reset here.
	recorded=$(dscl . -read "/Users/$slot" NFSHomeDirectory 2>/dev/null) ||
		refuse "cannot read the home of $slot"
	[ "$recorded" = "NFSHomeDirectory: $home" ] || refuse "the home of $slot is not $home"
}

# select_ports sets port1 to port4 and ports_csv from a ports.conf line's
# ports: exactly four, each a decimal from 1024 to 65535 with no leading
# zero. Globbing is off while the line splits on spaces.
select_ports() {
	set -f
	saved_ifs=$IFS
	IFS=' '
	# shellcheck disable=SC2086 # the line splits into its ports
	set -- $1
	IFS=$saved_ifs
	set +f
	[ "$#" -eq 4 ] || refuse "slot $slot does not have four ports in $portsconf"
	for port in "$@"; do
		case $port in
		'' | 0* | *[!0-9]*) refuse "malformed port $port for $slot in $portsconf" ;;
		esac
		[ "${#port}" -le 5 ] && [ "$port" -ge 1024 ] && [ "$port" -le 65535 ] ||
			refuse "port $port for $slot in $portsconf is outside 1024-65535"
	done
	port1=$1
	port2=$2
	port3=$3
	port4=$4
	ports_csv=$1,$2,$3,$4
}

# caller_owned refuses a path that is not owned by the caller or that group
# or others may write.
caller_owned() {
	meta=$(stat -f '%u %Lp' "$1") || refuse "cannot inspect $1"
	owner=${meta% *}
	perm=${meta#* }
	[ "$owner" = "$caller" ] || refuse "$1 is not owned by the caller"
	[ $((0$perm & 022)) -eq 0 ] || refuse "$1 is writable by group or others"
}

# select_swift_cache sets entry for a Swift dependency cache key. The cache
# belongs to the caller and is read as the caller, as the workspace is, so
# the check is that no one else, a slot included, could have written the
# directories the seed is read from, and that the entry is complete.
select_swift_cache() {
	key=$1
	case $key in
	*[!0-9a-f]*) refuse "malformed Swift cache key" ;;
	esac
	[ "${#key}" -eq 64 ] || refuse "malformed Swift cache key"
	entry=$root/swift-cache/$key
	for dir in "$root/swift-cache" "$entry" "$entry/seed" "$entry/seed/job" "$entry/seed/home"; do
		[ -d "$dir" ] && [ ! -L "$dir" ] || refuse "$dir is not a directory"
		caller_owned "$dir"
	done
	[ -f "$entry/record.json" ] && [ ! -L "$entry/record.json" ] ||
		refuse "Swift cache entry $key is incomplete"
}

# select_go_cache sets go_entry and goroot for a Go module cache key, on
# the same terms as select_swift_cache: no one but the caller could have
# written the directories the command reads, and the entry is complete.
select_go_cache() {
	go_key=$1
	case $go_key in
	*[!0-9a-f]*) refuse "malformed Go cache key" ;;
	esac
	[ "${#go_key}" -eq 64 ] || refuse "malformed Go cache key"
	go_entry=$root/go-cache/$go_key
	goroot=$root/go-cache/go$GO_VERSION
	for dir in "$root/go-cache" "$go_entry" "$go_entry/modcache" "$goroot" "$goroot/bin"; do
		[ -d "$dir" ] && [ ! -L "$dir" ] || refuse "$dir is not a directory"
		caller_owned "$dir"
	done
	[ -f "$go_entry/record.json" ] && [ ! -L "$go_entry/record.json" ] ||
		refuse "Go cache entry $go_key is incomplete"
	[ -f "$goroot/bin/go" ] && [ ! -L "$goroot/bin/go" ] || refuse "the Go $GO_VERSION toolchain is incomplete"
}

select_job() {
	job=$1
	case $job in
	*[!0-9a-f]*) refuse "malformed job id" ;;
	esac
	[ "${#job}" -eq 32 ] || refuse "malformed job id"
	jobdir=$jobs/$job
	profile=$jobs/$job.sb
	archive=$jobs/$job.tar
	gorecord=$jobs/$job.go
}

# live_count prints how many processes the slot owns, by real or effective uid.
live_count() {
	n=0
	for flag in -U -u; do
		found=0
		pgrep "$flag" "$uid" >/dev/null || found=$?
		case $found in
		0) n=$((n + 1)) ;;
		1) ;;
		*) refuse "cannot list processes of $slot" ;;
		esac
	done
	echo "$n"
}

assert_idle() {
	[ "$(live_count)" -eq 0 ] || refuse "slot $slot owns a live process"
}

# user_dir resolves the slot's per-user temporary or cache directory to its
# canonical /private/var/folders path, which is the path the sandbox matches.
user_dir() {
	reported=$(sudo -u "$slot" /usr/bin/getconf "$1") || refuse "cannot resolve $1 for $slot"
	reported=${reported%/}
	case $reported in
	/var/folders/??/*/[TC]) ;;
	*) refuse "unexpected $1 $reported" ;;
	esac
	middle=${reported#/var/folders/??/}
	middle=${middle%/?}
	case $middle in
	'' | *[!A-Za-z0-9_+-]*) refuse "unexpected $1 $reported" ;;
	esac
	resolved=/private$reported
	[ -d "$resolved" ] && [ ! -L "$resolved" ] || refuse "$resolved is not a directory"
	[ "$(cd -P "$resolved" && pwd -P)" = "$resolved" ] || refuse "$resolved is not canonical"
	[ "$(stat -f '%u' "$resolved")" = "$uid" ] || refuse "$resolved is not owned by $slot"
	echo "$resolved"
}

select_user_dirs() {
	tmp=$(user_dir DARWIN_USER_TEMP_DIR)
	cache=$(user_dir DARWIN_USER_CACHE_DIR)
}

# remove deletes a tree the slot may have written. The slot first clears the
# user immutable and append-only flags it may have set on its own files (as
# the slot, so a hard link to someone else's file is untouched); root then
# removes the tree. rm never follows a symlink inside it.
remove() {
	if [ -e "$1" ] || [ -L "$1" ]; then
		sudo -u "$slot" /usr/bin/chflags -R -P nouchg,nouappnd "$1" 2>/dev/null || :
		rm -rf "$1"
	fi
}

empty_dir() {
	sudo -u "$slot" /usr/bin/chflags -R -P nouchg,nouappnd "$1" 2>/dev/null || :
	find "$1" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
}

# macOS leaves entries in a slot's per-user temporary directory that neither
# root nor the slot can remove (#3009): trustd's com.apple.trustd and
# com.apple.MTLCompilerService carry System Integrity Protection, and the
# TemporaryItems directories (one at the top, one in com.apple.trustd) can be
# neither listed nor removed. The slot can write inside all four, and a file
# a job leaves in a TemporaryItems directory reads back in the next job. So
# each survivor is tolerated only when it is on this list, is a directory,
# and is empty: the listable ones by their listing, trustd's TemporaryItems
# by its link count, which APFS raises by one for every entry. Anything a
# job leaves in them fails cleanup, and the slot is quarantined instead of
# carrying it to the next job.
#
# The temporary directory's top-level TemporaryItems is the exception: it is
# tolerated with entries in it. It is Foundation's item-replacement
# directory, and a SwiftPM atomic write sometimes leaves an entry there that
# no one can remove, which would quarantine the slot for good. No one can
# list it and Foundation names each entry at random, so a later job reaches
# an entry only by its name: a planted entry only by a job written to
# cooperate with the one that left it, and an honest job's leftover only by
# guessing its random name. Nothing bounds or measures what accumulates
# there, so a job can fill the disk. The owner accepted both
# (devlog/2026-11-11-2151-macverify-tolerate-temporary-items.md). The cache
# directory's TemporaryItems, if any, stays strict.

# These functions run inside prepare and cleanup, and sh has no local
# variables, so theirs carry a ud_ prefix that nothing else uses.

# listing sets ud_entries to the names in directory $1, or refuses.
listing() {
	ud_entries=$(find "$1" -mindepth 1 -maxdepth 1) || refuse "cannot list $1"
}

# unlistable_empty refuses unless $1 is a directory with no entries.
unlistable_empty() {
	[ -d "$1" ] && [ ! -L "$1" ] || refuse "$1 is not a directory"
	[ "$(stat -f '%l' "$1")" -eq 2 ] || refuse "$1 holds entries no one can list or remove"
}

# empty_user_dir empties the slot's per-user temporary ($2 = temp) or cache
# ($2 = cache) directory $1, leaving only the protected entries above: each
# empty, except the temporary directory's top-level TemporaryItems.
empty_user_dir() {
	sudo -u "$slot" /usr/bin/chflags -R -P nouchg,nouappnd "$1" 2>/dev/null || :
	# rm fails on the protected entries; what survives is checked below.
	find "$1" -mindepth 1 -maxdepth 1 -exec rm -rf {} + 2>/dev/null || :
	listing "$1"
	ud_left=$ud_entries
	ud_allowed=
	for ud_name in com.apple.trustd com.apple.MTLCompilerService TemporaryItems; do
		ud_entry=$1/$ud_name
		[ -e "$ud_entry" ] || [ -L "$ud_entry" ] || continue
		[ -d "$ud_entry" ] && [ ! -L "$ud_entry" ] || refuse "$ud_entry is not a directory"
		case $ud_name in
		com.apple.trustd)
			listing "$ud_entry"
			if [ -n "$ud_entries" ]; then
				[ "$ud_entries" = "$ud_entry/TemporaryItems" ] || refuse "cannot empty $ud_entry"
				unlistable_empty "$ud_entry/TemporaryItems"
			fi
			;;
		com.apple.MTLCompilerService)
			listing "$ud_entry"
			[ -z "$ud_entries" ] || refuse "cannot empty $ud_entry"
			;;
		TemporaryItems)
			[ "$2" = temp ] || unlistable_empty "$ud_entry"
			;;
		esac
		ud_allowed=${ud_allowed:+$ud_allowed
}$ud_entry
	done
	# find lists in directory order, not the order above, so compare sorted.
	[ "$(printf '%s\n' "$ud_left" | sort)" = "$(printf '%s\n' "$ud_allowed" | sort)" ] ||
		refuse "cannot empty $1: left $(printf '%s' "$ud_left" | tr '\n' ' ')"
}

reset_home() {
	remove "$home"
	ditto "$root/template/home" "$home"
	chown -R -P "$slot" "$home"
	chmod 0700 "$home"
}

op_prepare() {
	usage="usage: prepare <slot> <job-id> <workdir> [swift=<key>] [go=<key>]"
	[ "$#" -ge 3 ] || refuse "$usage"
	select_slot "$1"
	select_job "$2"
	src=$3
	shift 3
	entry=
	go_key=
	case ${1:-} in
	swift=*)
		select_swift_cache "${1#swift=}"
		shift
		;;
	esac
	case ${1:-} in
	go=*)
		select_go_cache "${1#go=}"
		shift
		;;
	esac
	[ "$#" -eq 0 ] || refuse "$usage"
	case $src in
	/*) ;;
	*) refuse "workdir must be absolute" ;;
	esac
	# Checked as the caller, so root answers nothing about a path the
	# caller could not see.
	sudo -u "#$caller" test -d "$src" || refuse "workdir is not a directory"
	assert_idle
	if [ ! -d "$jobs" ]; then
		mkdir -m 0755 "$jobs"
	fi
	[ ! -L "$jobs" ] || refuse "$jobs is a symlink"
	root_owned "$jobs"
	# A slot runs one job at a time, so anything here is left from a job
	# whose cleanup never ran (one the daemon abandoned or a restart cut off).
	empty_dir "$jobs"
	head -c "$((MAX_PROFILE_BYTES + 1))" >"$profile"
	size=$(wc -c <"$profile")
	[ "$size" -gt 0 ] || refuse "empty sandbox profile"
	[ "$size" -le "$MAX_PROFILE_BYTES" ] || refuse "sandbox profile exceeds $MAX_PROFILE_BYTES bytes"
	chmod 0644 "$profile"
	if [ -n "$go_key" ]; then
		printf '%s\n' "$go_key" >"$gorecord"
		chmod 0644 "$gorecord"
	fi
	select_user_dirs
	reset_home
	empty_user_dir "$tmp" temp
	empty_user_dir "$cache" cache
	mkdir -m 0700 "$jobdir"
	chown "$slot" "$jobdir"
	sudo -u "$slot" mkdir "$jobdir/workspace"
	# Apply the profile once with the job's parameters, so a profile that
	# does not compile is a refusal here and not the command's exit status.
	sudo -u "$slot" /usr/bin/sandbox-exec -f "$profile" \
		-D "JOB_DIR=$jobdir" -D "SLOT_HOME=$home" -D "SLOT_TMP=$tmp" -D "SLOT_CACHE=$cache" \
		-D "PORT_1=$port1" -D "PORT_2=$port2" -D "PORT_3=$port3" -D "PORT_4=$port4" \
		-- /usr/bin/true || refuse "the sandbox profile does not apply"
	# The caller reads its own workspace and the slot writes the copy, so
	# root neither reads a path the caller could not nor writes one the slot
	# could not. tar keeps symlinks as symlinks and refuses .. and paths
	# through symlinks on extraction. Root opens both ends of the archive
	# on purpose (SC2024): neither the caller nor the slot may write there.
	# shellcheck disable=SC2024
	sudo -u "#$caller" tar -c -f - --no-xattrs --no-acls --no-fflags --no-mac-metadata \
		-C "$src" . >"$archive"
	# shellcheck disable=SC2024
	sudo -u "$slot" tar -x -f - --no-xattrs --no-acls --no-fflags --no-mac-metadata \
		-C "$jobdir/workspace" <"$archive"
	swift_cache=none
	if [ -n "$entry" ]; then
		# The seed goes the way the workspace did. It names its members, so
		# no archive entry for . resets the job directory's or the home's
		# mode, and tar's refusal to extract through a symlink stops a
		# workspace that makes a package's .swiftpm a symlink from
		# redirecting the write.
		# shellcheck disable=SC2024
		sudo -u "#$caller" tar -c -f - --no-xattrs --no-acls --no-fflags --no-mac-metadata \
			-C "$entry/seed/job" workspace source-packages >"$archive"
		# shellcheck disable=SC2024
		sudo -u "$slot" tar -x -f - --no-xattrs --no-acls --no-fflags --no-mac-metadata \
			-C "$jobdir" <"$archive"
		# shellcheck disable=SC2024
		sudo -u "#$caller" tar -c -f - --no-xattrs --no-acls --no-fflags --no-mac-metadata \
			-C "$entry/seed/home" .gitconfig >"$archive"
		# shellcheck disable=SC2024
		sudo -u "$slot" tar -x -f - --no-xattrs --no-acls --no-fflags --no-mac-metadata \
			-C "$home" <"$archive"
		swift_cache=$key
	fi
	rm -f "$archive"
	assert_idle
	printf 'root=%s\nuid=%s\nhome=%s\ntmp=%s\ncache=%s\nports=%s\nswift_cache=%s\ngo_cache=%s\n' \
		"$root" "$uid" "$home" "$tmp" "$cache" "$ports_csv" "$swift_cache" "${go_key:-none}"
}

op_run() {
	[ "$#" -ge 17 ] || refuse "usage: run <slot> <job-id> <environment> <parameters> -- <argv>"
	select_slot "$1"
	select_job "$2"
	shift 2
	select_user_dirs
	[ -d "$jobdir" ] && [ ! -L "$jobdir" ] || refuse "$jobdir is not a directory"
	[ "$(stat -f '%u %Lp' "$jobdir")" = "$uid 700" ] || refuse "$jobdir is not $slot's at 0700"
	[ -f "$profile" ] && [ ! -L "$profile" ] || refuse "$profile is not a regular file"
	root_owned "$profile"
	# A job prepared with a Go cache gets the pinned toolchain first on its
	# PATH and the Go environment, one newline-separated assignment per line,
	# from the record prepare left. Every value is built from the checked
	# root and key, so word splitting on IFS (a newline) yields exactly those
	# assignments and no pattern characters.
	command_path=$COMMAND_PATH
	go_env=
	if [ -e "$gorecord" ] || [ -L "$gorecord" ]; then
		[ -f "$gorecord" ] && [ ! -L "$gorecord" ] || refuse "$gorecord is not a regular file"
		root_owned "$gorecord"
		select_go_cache "$(cat "$gorecord")"
		command_path=$goroot/bin:$COMMAND_PATH
		go_env="GOFLAGS=-mod=readonly
GOMODCACHE=$go_entry/modcache
GOPROXY=off
GOTOOLCHAIN=local
GOWORK=off"
	fi
	# The daemon names the environment and sandbox parameters it expects;
	# they must be exactly the ones this script derives.
	# shellcheck disable=SC2086 # go_env splits into its assignments (above)
	for want in "HOME=$home" "TMPDIR=$tmp" "PATH=$command_path" "LC_ALL=C" \
		"FREESIDE_VERIFY_PORTS=$ports_csv" $go_env \
		"JOB_DIR=$jobdir" "SLOT_HOME=$home" "SLOT_TMP=$tmp" "SLOT_CACHE=$cache" \
		"PORT_1=$port1" "PORT_2=$port2" "PORT_3=$port3" "PORT_4=$port4" --; do
		[ "$1" = "$want" ] || refuse "expected $want, got $1"
		shift
	done
	[ "$#" -ge 1 ] || refuse "empty command"
	cd "$jobdir/workspace" || refuse "cannot enter $jobdir/workspace"
	# Spike 1: only the slot's own launchd context is free of the operator's
	# window-server session.
	# shellcheck disable=SC2086 # go_env splits into its assignments (above)
	exec launchctl asuser "$uid" sudo -u "$slot" \
		/usr/bin/env -i "HOME=$home" "TMPDIR=$tmp" "PATH=$command_path" LC_ALL=C \
		"FREESIDE_VERIFY_PORTS=$ports_csv" $go_env \
		/usr/bin/sandbox-exec -f "$profile" \
		-D "JOB_DIR=$jobdir" -D "SLOT_HOME=$home" -D "SLOT_TMP=$tmp" -D "SLOT_CACHE=$cache" \
		-D "PORT_1=$port1" -D "PORT_2=$port2" -D "PORT_3=$port3" -D "PORT_4=$port4" \
		-- "$@"
}

op_reap() {
	[ "$#" -eq 1 ] || refuse "usage: reap <slot>"
	select_slot "$1"
	attempt=0
	while [ "$attempt" -lt 50 ]; do
		# kill -1 as the slot signals every process the slot may signal in
		# one pass, which a fork loop cannot outrun; pkill as root also
		# catches a set-uid child whose real uid is the slot's.
		sudo -u "$slot" /bin/kill -s KILL -- -1 2>/dev/null || :
		pkill -9 -U "$uid" 2>/dev/null || :
		pkill -9 -u "$uid" 2>/dev/null || :
		# Stop anything the slot submitted to its own launchd domain, which
		# would otherwise respawn after the kill.
		launchctl bootout "user/$uid" >/dev/null 2>&1 || :
		if [ "$(live_count)" -eq 0 ]; then
			printf 'uid=%s\n' "$uid"
			return 0
		fi
		sleep 0.1
		attempt=$((attempt + 1))
	done
	refuse "slot $slot still owns a process after reaping"
}

op_cleanup() {
	[ "$#" -eq 2 ] || refuse "usage: cleanup <slot> <job-id>"
	select_slot "$1"
	select_job "$2"
	assert_idle
	remove "$jobdir"
	rm -f "$profile" "$archive" "$gorecord"
	select_user_dirs
	reset_home
	empty_user_dir "$tmp" temp
	empty_user_dir "$cache" cache
	assert_idle
}

[ "$#" -ge 1 ] || refuse "usage: slot-run prepare|run|reap|cleanup …"
operation=$1
shift
case $operation in
prepare) op_prepare "$@" ;;
run) op_run "$@" ;;
reap) op_reap "$@" ;;
cleanup) op_cleanup "$@" ;;
*) refuse "unknown operation $operation" ;;
esac
