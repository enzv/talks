/*
 * Allocates 1 MiB chunks until OOME, then sleeps to keep running.
 *
 * Demonstrates how legacy JVMs mis-detect cgroup v2 limits compared to patched
 * JDKs. The workload stays identical so any difference in behavior comes from
 * the JDK build, not the program.
 *
 * Grows heap unbounded, triggers either a Java OOME (expected) or a kernel OOM
 * kill (broken), then pauses so you can inspect logs, describe the pod, or
 * attach debugging tools.
 */
import java.util.ArrayList;

public final class MemHog {
  public static void main(String[] args) throws Exception {
    ArrayList<byte[]> blocks = new ArrayList<byte[]>();
    try {
      while (true) blocks.add(new byte[1 << 20]);
    } catch (OutOfMemoryError oom) {
      Thread.sleep(3600_000L);
    }
  }
}
