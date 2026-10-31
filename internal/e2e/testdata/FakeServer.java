import java.io.BufferedReader;
import java.io.IOException;
import java.io.InputStreamReader;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.nio.file.StandardOpenOption;
import java.time.LocalTime;
import java.time.format.DateTimeFormatter;

/**
 * A stand-in for a Minecraft 1.13+ server, for the native end-to-end tests of
 * the manager. It is a real JVM process started by "java -jar" in a real GNU
 * screen session, and speaks the part of the console protocol the manager
 * relies on: it logs a "Done" line when ready, answers save-all, save-off,
 * save-on, say, list and whitelist, and exits on stop. It writes
 * logs/latest.log like the vanilla server and keeps its world in world/.
 *
 * It does not speak the network protocol and is not a Minecraft server; see
 * docs/validation.md for what that means for the validation claims.
 */
public class FakeServer {
    private static final DateTimeFormatter TIME = DateTimeFormatter.ofPattern("HH:mm:ss");
    private static final Path LOG = Path.of("logs", "latest.log");
    private static final Path LEVEL = Path.of("world", "level.dat");

    private static void log(String message) throws IOException {
        Files.writeString(LOG, "[" + LocalTime.now().format(TIME) + "] [Server thread/INFO]: " + message + "\n",
                StandardOpenOption.CREATE, StandardOpenOption.APPEND);
    }

    private static void writeLevel(String text) throws IOException {
        Files.createDirectories(LEVEL.getParent());
        Files.writeString(LEVEL, text);
    }

    public static void main(String[] args) throws Exception {
        Files.createDirectories(LOG.getParent());
        if (Files.exists(LOG)) {
            Files.move(LOG, Path.of("logs", "previous.log"), StandardCopyOption.REPLACE_EXISTING);
        }
        log("Starting fake server pid=" + ProcessHandle.current().pid() + " java=" + System.getProperty("java.version"));
        if (!Files.exists(LEVEL)) {
            writeLevel("new world");
        }
        Thread.sleep(300);
        log("Done (0.3s)! For help, type \"help\"");

        BufferedReader in = new BufferedReader(new InputStreamReader(System.in));
        String line;
        while ((line = in.readLine()) != null) {
            line = line.trim();
            if (line.equals("save-all")) {
                log("Saved the game");
            } else if (line.equals("save-off")) {
                log("Turned off world auto-saving");
            } else if (line.equals("save-on")) {
                log("Turned on world auto-saving");
            } else if (line.startsWith("say ")) {
                log("[Server] " + line.substring(4));
            } else if (line.equals("list")) {
                log("There are 0 of a max of 20 players online:");
            } else if (line.equals("whitelist on")) {
                log("Whitelist is now turned on");
            } else if (line.startsWith("world-write ")) {
                // Test hook: player progress lands in the world.
                writeLevel(line.substring("world-write ".length()));
                log("[Server] wrote the world");
            } else if (line.equals("stop")) {
                log("Stopping server");
                System.exit(0);
            }
        }
    }
}
