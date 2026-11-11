# Arduino Flasher CLI

A tool to download and flash Debian images on the board.

## Docs

For a full guide on how to use it, see the [User documentation](https://docs.arduino.cc/tutorials/uno-q/update-image/).

## Build and test it locally

Build it with `task build` and run:

```sh
# Flash the most recent Debian image for the UNO Q
./build/arduino-flasher-cli flash unoq

# Flash a local image. It works with either an archived or extracted image
./build/arduino-flasher-cli flash path/to/downloaded/image
```

## Download images

`arduino-flasher-cli download unoq` saves the image archive in the user cache
under `arduino-flasher-cli/images/`. Each invocation reads the online manifest
and reuses a cached archive only if its SHA-256 checksum matches the release.

Use `--dest-dir` to download to an existing directory instead of the cache:

```sh
arduino-flasher-cli download unoq --dest-dir .
```

## Reuse images when flashing

`arduino-flasher-cli flash unoq` checks the online manifest and reuses a cached
archive when its checksum matches. If no valid archive is cached, the image is
downloaded temporarily and removed after the operation, including on failure.

Use `--cache` to retain newly downloaded archives for future flashes:

```sh
arduino-flasher-cli flash unoq --cache
```

Interactive image selections mark matching local archives with `(cached)`. The
flash wizard also asks whether to keep new downloads. Existing cached archives
are retained regardless of this choice, including when flashing fails.
Extraction always uses a temporary directory. `--temp-dir` selects its parent;
cached archives stay in the user cache. Local image files and directories are
flashed directly without being added to the cache.
When reusing a verified cached archive, the free-space check only accounts for
extraction.

## Manage cached images

Inspect or clear the shared image cache with:

```sh
arduino-flasher-cli cache list
arduino-flasher-cli cache list --format json
arduino-flasher-cli cache clear
```

`cache list` works offline and reports each complete image's board, distribution,
version, archive size, download date and path (in JSON output). Total cache size
includes metadata and incomplete downloads. Listing does not recheck archive
checksums; those are verified against the online manifest before reuse.

`cache clear` immediately removes images and incomplete downloads from the
`images/` cache directory. Empty synchronization files in `.locks/` are preserved
so concurrent processes continue to share the same publication locks.
Files downloaded to another directory with `--dest-dir` and temporary directories
outside `images/` are preserved. Verified archives and metadata are published
together by renaming their complete temporary directory.
Both commands also work when the cache has not been created yet.

The normal `list` command marks downloaded images in the `CACHED` column and
includes a `cached` boolean in JSON output. It compares local metadata with the
online image's board, distribution, version, checksum and archive name, without
reading archive contents. A matching image with same-size corruption can still
appear cached; its checksum is verified before reuse by `download` or `flash`.
If the cache cannot be inspected, `list` reports a warning and still lists the
online images without cache marks.

## Cache behavior over gRPC

The `Flash` RPC uses the same image cache as the CLI. It reads the online manifest
and verifies cached archives before reuse. Set `FlashRequest.cache` to `true` to
retain new downloads; the default is `false`, matching `flash` without `--cache`.
Cached archives survive both successful and failed flashes.

`FlashRequest.temp_path` selects the parent of a private per-request directory.
An empty path uses the user cache directory. This private directory is removed
on success, failure or cancellation; existing files in the parent are preserved.
This replaces the previous behavior of retaining downloads there after a failed
flash. Clients that need persistent images should opt in with `cache`.

Download start/end events are also sent on a cache hit, with a completed progress
update and a `Using cached image` end message. Extraction and flash progress use
the existing response messages.

## Read an additional index

Only the published indexes are read by default. `ARDUINO_FLASHER_ADDITIONAL_URLS`
adds more, separated by commas or newlines, and they are read _alongside_ the
published ones rather than instead of them:

```sh
ARDUINO_FLASHER_ADDITIONAL_URLS="http://127.0.0.1:3001/debian-im/Stable/info.json" \
  ./build/arduino-flasher-cli list
```

The same can be done for one command with `--additional-url` (repeat the flag
to add more than one index):

```sh
./build/arduino-flasher-cli flash unoq --additional-url https://example.com/info.json
```

The URL must point to an image index in the same format as the published
`info.json` files. Using `--additional-url` prints a warning: images from these
sources are unofficial and have not been validated by Arduino. Download and
flash them only if you trust the source.

`ARDUINO_FLASHER_ADDITIONAL_HEADERS` adds HTTP headers, one `Name: value` per
line, sent both when reading an index and when downloading the image it points
at, for a host that asks to be authenticated:

```sh
ARDUINO_FLASHER_ADDITIONAL_HEADERS="Authorization: Bearer $TOKEN" \
  ./build/arduino-flasher-cli list
```

An index that cannot be read is reported as a warning and the others are still
listed. One that is not published yet is passed over in silence.

The `list` command shows UNO Q and VENTUNO Q images together in one table by
default, without source headings or URLs. Supplying `--additional-url` enables
numbered source sections: one combined official Arduino table, followed by each
additional index. All matching boards and distributions stay together within
each table. The official heading is bold green on color-capable terminals;
additional headings are bold yellow and marked `UNOFFICIAL / UNVALIDATED`.
Separators and labels remain clear without color. Official index URLs are omitted
from text output; each additional index URL appears once in its heading, with
credentials and query strings redacted.
Separate section numbers distinguish indexes that differ only by redacted
parameters. Environment-provided indexes still participate in the listing, but
source headings are enabled only by the command-line `--additional-url` flag.

JSON output remains a flat `releases` array and includes `source_url` and
`official` on each image. JSON retains full source URLs, so treat it as sensitive
if your index URLs contain credentials or tokens. Official status is assigned
from the built-in index configuration, never from a manifest's claimed publisher.
Board IDs, cache identity and image selection are unchanged. In CLI text and JSON
list output, `LATEST` marks the newest image within each source for each board
and distribution. Official and additional sources can both have latest markers.
Download and flash still resolve versions across the configured sources; these
per-source listing markers do not change which image is selected automatically.

## Create and edit image manifests

The `manifest` commands write the same JSON format as the published `info.json`
indexes. Create an empty manifest or build one from local image files, a directory,
or a list of HTTP(S) image URLs:

```sh
arduino-flasher-cli manifest create --output info.json
arduino-flasher-cli manifest create ./images --base-url https://example.com/images/ --output info.json
arduino-flasher-cli manifest create https://example.com/unoq-debian-20261101-1.tar.zst https://example.com/unoq-debian-20261102-1.tar.zst --output info.json
```

Local files require `--base-url`, the published directory where those files will
be hosted. Each filename is appended to that URL, with spaces and other special
characters escaped. This command does **not** upload files. Directory scans are
not recursive and include image archives (`.tar`, `.tar.zst`, `.tar.gz`, `.tgz`,
`.tar.xz`, `.tar.bz2`, `.zip`) and `.img` files, including compressed `.img` files.
Other files, subdirectories and symlinks inside the directory are skipped.
Individual files can be supplied explicitly regardless of their extension.

Board, OS and versions such as `20261101-1` are detected from filenames. Unknown
or ambiguous metadata produces a warning and an interactive prompt. For scripts,
or to override detection, supply `--board`, `--os` and `--version`. These flags
apply to every image added by that invocation:

```sh
arduino-flasher-cli manifest add ./custom-image.zip --manifest info.json --base-url https://example.com/images/ --board ventunoq --os "Debian GNU/Linux 13 (trixie)" --version 20261101-1
```

SHA-256 is calculated from the complete file contents. Remote images are streamed
in full without being saved; this can use significant bandwidth. The existing
`ARDUINO_FLASHER_ADDITIONAL_HEADERS` setting also applies to these requests.
HTTP errors, empty files and HTML responses are rejected. Filename detection and
checksums do not validate that an image is safe or suitable for the board.

Remove an entry by its exact URL, or by its local filename with `--base-url`:

```sh
arduino-flasher-cli manifest remove https://example.com/images/custom-image.zip --manifest info.json
```

Removal only edits the manifest; it does not delete local or remote images.
Create refuses to overwrite an existing manifest. Failed edits leave the existing
file unchanged. Edit a manifest from only one process at a time. Files with
unrecognized JSON fields are rejected rather than silently losing those fields.

### Select the latest entry

Use `--latest <URL>` with `create`, `add` or `remove`, or select an existing URL
without hashing or downloading it again:

```sh
arduino-flasher-cli manifest latest https://example.com/images/custom-image.zip --manifest info.json
```

The selected URL must be present in the resulting manifest. New manifests default
to the newest version. Additions preserve the existing selection; removing the
selected image picks the newest remaining version unless `--latest` is supplied.
Empty manifests have an empty `releases` array and omit `latest`.

This sets the manifest's top-level `latest` block. The flasher's existing image
selection remains version-based for each board and distribution; it does not
use that block to override its selection.

## Update QDL version

flasher-cli embeds a statically builded [qdl](https://github.com/linux-msm/qdl) binary taken from the https://github.com/arduino/qdl-packing repo. To update the qdl version you need to:

1. Make sure the qdl-packing repo has the desired version of qdl (check the release page [here](https://github.com/arduino/qdl-packing/releases)) if not, create a PR to update it.

2. Update the `TAG` variable in script `internal/artifacts/download_resources.sh` to the desired version.

3. Test that everything works by running `task clean build` and testing the resulting binary.

4. Create a new release of flasher-cli, the build process will automatically download and embed the new qdl binary.
