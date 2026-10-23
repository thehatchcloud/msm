#!/bin/sh
# Builds the WorldEdit snapshot-lookup harness used by the backup
# acceptance test (internal/backup, TestWorldEditSnapshotAcceptance).
#
# It downloads the unmodified WorldEdit source files that implement snapshot
# discovery (FileSystemSnapshotDatabase and the archive/date helpers it
# uses) at a pinned release commit, plus Guava and JSR-305 from Maven
# Central (SHA-256 checked), compiles them with the small stubs in stubs/
# (they replace only world-reading and logging code that discovery never
# calls), and compiles Probe.java. Nothing is vendored: WorldEdit is GPLv3
# and is only fetched into the output directory.
#
# Usage: build.sh OUTDIR     (needs curl, javac, sha256sum)
# Then:  MSM_WORLDEDIT_HARNESS=OUTDIR go test ./internal/backup
set -eu

out=${1:?usage: build.sh OUTDIR}
here=$(cd "$(dirname "$0")" && pwd)

# Head of WorldEdit's version/7.3.x branch when this was written
commit=e2da7c111ec97feb459fc53fc5a6bf0c494b6e75
raw=https://raw.githubusercontent.com/EngineHub/WorldEdit/$commit/worldedit-core/src/main/java

guava_url=https://repo.maven.apache.org/maven2/com/google/guava/guava/33.4.0-jre/guava-33.4.0-jre.jar
guava_sha=b918c98a7e44dbe94ebd9fe3e40cddaadb5a93e6a78eb6008b42df237241e538
jsr_url=https://repo.maven.apache.org/maven2/com/google/code/findbugs/jsr305/3.0.2/jsr305-3.0.2.jar
jsr_sha=766ad2a0783f2687962c8ad74ceecc38a28b9f72a2d085ee438b7813e928d0c7

mkdir -p "$out/src" "$out/classes"
fetch() { # url sha file
	curl -fsSL -m 60 "$1" -o "$3"
	echo "$2  $3" | sha256sum -c - >/dev/null
}
fetch "$guava_url" "$guava_sha" "$out/guava.jar"
fetch "$jsr_url" "$jsr_sha" "$out/jsr305.jar"

for f in \
	util/function/IOFunction util/function/IORunnable util/io/Closer \
	util/io/file/ArchiveDir util/io/file/ArchiveNioSupport util/io/file/ArchiveNioSupports \
	util/io/file/MorePaths util/io/file/SafeFiles util/io/file/ZipArchiveNioSupport \
	util/io/file/AttributeTarget util/collection/MoreStreams \
	util/time/FileNameDateTimeParser util/time/ModificationDateTimeParser util/time/SnapshotDateTimeParser \
	world/snapshot/experimental/SnapshotDatabase world/snapshot/experimental/SnapshotInfo \
	world/snapshot/experimental/SnapshotComparator \
	world/snapshot/experimental/fs/FileSystemSnapshotDatabase
do
	mkdir -p "$out/src/com/sk89q/worldedit/$(dirname "$f")"
	curl -fsSL -m 60 "$raw/com/sk89q/worldedit/$f.java" -o "$out/src/com/sk89q/worldedit/$f.java"
done

javac -nowarn -cp "$out/guava.jar:$out/jsr305.jar" -d "$out/classes" \
	$(find "$out/src" "$here/stubs" -name '*.java') "$here/Probe.java"
echo "built $out"
