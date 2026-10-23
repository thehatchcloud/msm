package com.sk89q.worldedit.world.snapshot.experimental.fs;
import com.sk89q.worldedit.util.io.Closer;
import com.sk89q.worldedit.world.snapshot.experimental.*;
import java.nio.file.*;
public class FolderSnapshot implements Snapshot {
  private final SnapshotInfo info; private final Path root; private final Closer closer;
  public FolderSnapshot(SnapshotInfo info, Path root, Closer closer) { this.info = info; this.root = root; this.closer = closer; }
  public SnapshotInfo getInfo() { return info; }
  public Path getRoot() { return root; }
  public boolean hasLevelDat() { return Files.isRegularFile(root.resolve("level.dat")); }
  public void close() throws java.io.IOException { if (closer != null) closer.close(); }
}
