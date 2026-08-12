// Shared node-test helpers for the @fuse/sdk suites (change 0056).
//
// spawnGoServer launches a Go test server under sdk/ts (e.g. ./test/statesrv), reads the
// `URL <url>` line it prints, and resolves with the base URL + a group-kill stop(). The
// helper is launched `detached: true` so it leads its own process group: `go run` execs a
// compiled grandchild, and killing the `go run` pid alone would orphan it. stop() kills the
// whole group (kill(-pid)).

import { spawn, type ChildProcess } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const sdkTsDir = resolve(here, "..");

export interface RunningServer {
  url: string;
  stop: () => void;
}

// spawnGoServer runs `go run <pkg>` (relative to sdk/ts) with the given extra env, and
// resolves once the server prints `URL <url>`.
export function spawnGoServer(
  pkg: string,
  env: Record<string, string> = {},
): Promise<RunningServer> {
  return new Promise((resolvePromise, rejectPromise) => {
    const proc: ChildProcess = spawn("go", ["run", pkg], {
      cwd: sdkTsDir,
      stdio: ["ignore", "pipe", "pipe"],
      detached: true,
      env: { ...process.env, ...env },
    });
    const killGroup = () => {
      try {
        if (proc.pid !== undefined) process.kill(-proc.pid, "SIGKILL");
      } catch {
        // already gone
      }
      try {
        proc.kill("SIGKILL");
      } catch {
        // already gone
      }
    };
    let stdout = "";
    let settled = false;
    const timer = setTimeout(() => {
      if (!settled) {
        settled = true;
        killGroup();
        rejectPromise(new Error("timed out waiting for server URL; stdout=" + stdout));
      }
    }, 30_000);
    timer.unref?.();

    proc.stdout?.on("data", (chunk: Buffer) => {
      stdout += chunk.toString();
      const match = stdout.match(/^URL (\S+)$/m);
      if (match && !settled) {
        settled = true;
        clearTimeout(timer);
        resolvePromise({ url: match[1], stop: killGroup });
      }
    });
    let stderr = "";
    proc.stderr?.on("data", (chunk: Buffer) => {
      stderr += chunk.toString();
    });
    proc.on("exit", (code) => {
      if (!settled) {
        settled = true;
        clearTimeout(timer);
        rejectPromise(new Error(`server exited (code ${code}) before URL; stderr=${stderr}`));
      }
    });
  });
}
