import com.sk89q.worldedit.util.io.file.ArchiveNioSupports;
import com.sk89q.worldedit.world.snapshot.experimental.*;
import com.sk89q.worldedit.world.snapshot.experimental.fs.*;
import java.nio.file.*;
import java.util.*;
import java.util.stream.*;
public class Probe {
  public static void main(String[] a) throws Exception {
    FileSystemSnapshotDatabase db = new FileSystemSnapshotDatabase(Paths.get(a[0]), ArchiveNioSupports.combined());
    List<Snapshot> snaps;
    try (Stream<Snapshot> s = db.getSnapshots(a[1])) { snaps = s.collect(Collectors.toList()); }
    for (Snapshot s : snaps) {
      FolderSnapshot f = (FolderSnapshot) s;
      System.out.println(s.getInfo().getDateTime().toLocalDateTime() + " " + s.getInfo().getDisplayName() + " level.dat=" + f.hasLevelDat() + " files=" + countFiles(f.getRoot()));
    }
    if (snaps.isEmpty()) { System.out.println("NONE"); System.exit(2); }
  }
  static long countFiles(Path p) throws Exception { try (Stream<Path> w = Files.walk(p)) { return w.filter(Files::isRegularFile).count(); } }
}
