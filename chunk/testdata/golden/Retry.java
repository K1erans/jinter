package billing;

import java.util.ArrayList;
import java.util.List;
import java.util.function.Supplier;

/** Retries flaky calls. */
public final class Retry {
    private final int attempts;

    public Retry(int attempts) {
        this.attempts = attempts;
    }

    public <T> T run(Supplier<T> fn) {
        RuntimeException last = null;
        for (int i = 0; i < attempts; i++) {
            try {
                return fn.get();
            } catch (RuntimeException e) {
                last = e;
                sleep(100L << i);
            }
        }
        throw last;
    }

    private static void sleep(long ms) {
        try {
            Thread.sleep(ms);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }

    public static List<Integer> range(int n) {
        List<Integer> out = new ArrayList<>();
        for (int i = 0; i < n; i++) {
            out.add(i);
        }
        return out;
    }

    enum Mode { FAST, SLOW }

    interface Clock {
        long now();
    }
}
