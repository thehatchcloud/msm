package com.sk89q.worldedit.internal.util;
import org.apache.logging.log4j.Logger;
public class LogManagerCompat {
  public static Logger getLogger() {
    return new Logger() {
      public void warn(String m, Throwable t) { System.err.println("WARN " + m + " " + t); }
      public void warn(String m) { System.err.println("WARN " + m); }
    };
  }
}
