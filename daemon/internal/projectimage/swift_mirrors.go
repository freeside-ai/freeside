package projectimage

// The two fixed scripts that bake a Swift image's dependency mirrors. They
// live in their own file so the macOS offline Swift cache (#2880) can lift
// them.
const (
	// swiftMirrorDiscoveryPath holds the discovery in the stage that runs it.
	// The image does not ship that stage.
	swiftMirrorDiscoveryPath = "/opt/freeside/project-toolchain/discover-swift-mirrors"
	// swiftMirrorBakePath holds the script that clones the mirrors, in both
	// the discovery stage and the image.
	swiftMirrorBakePath = "/opt/freeside/project-toolchain/bake-swift-mirrors"
	// swiftMirrorURLsPath is the discovered URL list, one per line, in the
	// discovery stage: the only thing the shipped stage reads from it.
	swiftMirrorURLsPath = "/opt/freeside/project-toolchain/swift-mirror-urls"
	// swiftMirrorListMount is where the shipped stage's bake reads that list,
	// through a read-only bind mount of the discovery stage, so no discovery
	// layer becomes part of the image.
	swiftMirrorListMount = "/tmp/freeside-swift-mirror-urls"
	// swiftMirrorRecordPath is the bake's record of the URLs it mirrored, the
	// validated lines of the list, kept in the image.
	swiftMirrorRecordPath = SwiftMirrorsPath + "/urls"
)

// swiftMirrorDiscovery finds the repositories a Swift package's dependency
// closure clones. It runs in a build stage the image does not ship, because
// it evaluates Package.swift and every dependency's manifest, as any resolve
// does, and SwiftPM runs manifest code unsandboxed on Linux: whatever that
// code writes stays in that stage. It works from the two baked seeds alone:
// a resolve loads only Package.swift and Package.resolved, never the
// package's targets. In order, it:
//
//  1. Refuses a Package.swift that declares a local path dependency, which
//     would make the image depend on files neither manifest binds.
//  2. Resolves the closure Package.resolved pins through the build proxy.
//  3. Lists every repository that resolve cloned, from the clones' own
//     origin URLs, and every submodule of each checkout, recursively, so
//     clones git made on SwiftPM's behalf are covered without intercepting
//     git. The list goes to swiftMirrorURLsPath.
//  4. Bakes mirrors from that list with swiftMirrorBake, as the image will.
//  5. Proves the mirrors alone serve the closure: a fresh resolve with every
//     proxy pointed at a closed port must succeed and leave Package.resolved
//     byte-identical to the seed.
const swiftMirrorDiscovery = `#!/usr/bin/busybox sh
set -eu
seed=/opt/freeside/project-seed
work=$(/usr/bin/busybox mktemp -d /tmp/freeside-swift-discovery.XXXXXX)
trap '/usr/bin/busybox rm -rf "$work"' EXIT

fresh_package() {
	/usr/bin/busybox rm -rf "$work/package" "$work/home"
	/usr/bin/busybox mkdir "$work/package" "$work/home"
	/usr/bin/busybox cp "$seed/Package.swift" "$seed/Package.resolved" "$work/package/"
}

fresh_package
HOME=$work/home swift package dump-package --package-path "$work/package" >"$work/manifest.json"
if ! jq -e '[.dependencies[] | select(has("fileSystem"))] | length == 0' \
	"$work/manifest.json" >/dev/null; then
	echo "Package.swift declares a local path dependency, which no project image serves" >&2
	exit 1
fi

HOME=$work/home swift package resolve --package-path "$work/package" \
	--only-use-versions-from-resolved-file
{
	for repository in "$work/package/.build/repositories/"*; do
		[ -d "$repository" ] || continue
		git -C "$repository" config --get remote.origin.url
	done
	for checkout in "$work/package/.build/checkouts/"*; do
		[ -d "$checkout" ] || continue
		git -C "$checkout" submodule foreach --quiet --recursive \
			'git config --get remote.origin.url'
	done
} | /usr/bin/busybox sort -u >/opt/freeside/project-toolchain/swift-mirror-urls
/usr/bin/busybox sh /opt/freeside/project-toolchain/bake-swift-mirrors \
	/opt/freeside/project-toolchain/swift-mirror-urls

fresh_package
closed=http://127.0.0.1:9
HOME=$work/home HTTP_PROXY=$closed HTTPS_PROXY=$closed http_proxy=$closed https_proxy=$closed \
	ALL_PROXY=$closed all_proxy=$closed \
	swift package resolve --package-path "$work/package" --only-use-versions-from-resolved-file
if ! /usr/bin/busybox cmp -s "$work/package/Package.resolved" "$seed/Package.resolved"; then
	echo "the baked mirrors resolve Package.resolved differently" >&2
	exit 1
fi
`

// swiftMirrorBake clones each URL in the list file named by its one argument
// once into SwiftMirrorsPath, under the SHA-256 of its spelling without a
// trailing .git, and writes the gitconfig the launcher points
// GIT_CONFIG_GLOBAL at: an insteadOf rewrite for both spellings of each URL,
// and file transport allowed for submodules. It records each URL it accepted
// in swiftMirrorRecordPath. It runs no project or dependency code, so the
// image runs it in a layer it ships.
//
// The list comes out of the discovery stage, where manifest code ran as root,
// so nothing about it is trusted. It must be a regular file: a directory
// would read as an empty list. Each line must be an https URL of printable
// characters with no space, and git config escapes every value it writes.
// Manifest code chooses the lines and their order, so a crafted line can
// claim a rewrite another dependency's URL also matches. That changes only
// which clone serves a fetch: every checkout must reach the exact revision
// Package.resolved pins, and the room compares Package.resolved with the
// baked seed after its resolve.
//
// git rewrites a URL by its longest matching insteadOf prefix, so a
// dependency whose URL merely begins with another's still reaches its own
// mirror, and one with no mirror is rewritten to a path that does not exist
// and fails offline.
const swiftMirrorBake = `#!/usr/bin/busybox sh
set -eu
list=$1
mirrors=/opt/freeside/swiftpm-mirrors
if [ ! -f "$list" ] || [ -L "$list" ]; then
	echo "the Swift mirror URL list is not a regular file" >&2
	exit 1
fi
/usr/bin/busybox mkdir "$mirrors"
git config --file "$mirrors/gitconfig" protocol.file.allow always
while IFS= read -r url; do
	case "$url" in
	https://*) ;;
	*)
		echo "Swift dependency $url is not an https URL" >&2
		exit 1
		;;
	esac
	if printf '%s' "$url" | /usr/bin/busybox grep -q '[^!-~]'; then
		echo "Swift dependency URL holds a space or control character" >&2
		exit 1
	fi
	printf '%s\n' "$url" >>"$mirrors/urls"
	key=${url%.git}
	name=$(printf '%s' "$key" | /usr/bin/busybox sha256sum | /usr/bin/busybox cut -c1-64).git
	if [ ! -d "$mirrors/$name" ]; then
		echo "mirroring $url"
		git clone --quiet --mirror -- "$url" "$mirrors/$name"
		git config --file "$mirrors/gitconfig" --add "url.file://$mirrors/$name.insteadOf" "$key"
		git config --file "$mirrors/gitconfig" --add "url.file://$mirrors/$name.insteadOf" "$key.git"
	fi
done <"$list"
`
