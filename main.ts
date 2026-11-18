// main.ts: the entry point the trial runs as `node /bot/main.ts`.
// A thin launcher — the whole forge is the Go binary next to it.
// It spawns ./forge, forwards stdio, and exits with the forge's code.
//
// The binary is shipped in the repository, but the upload paths used to
// push it (connector push + GitHub web upload) strip the executable bit:
// the tree on GitHub records `forge` as mode 100644 even though the local
// file is 100755. Spawning it then fails with EACCES and the forge never
// binds, which the trial reports as working=false.
//
// v2 tried chmod 0o755 in place before spawning. That works on a writable
// checkout, but the sealed trial mounts the checkout READ-ONLY: chmod fails
// with EROFS, the spawn then fails with EACCES, and the entry scores 0
// (proven by the parent from a read-only bind-mount: chmod EROFS, spawn
// EACCES, no register). v3 therefore climbs a fallback ladder:
//   (a) chmod in place + spawn the binary by absolute path;
//   (b) on failure, copy the binary to os.tmpdir(), chmod the copy, spawn it;
//   (c) on failure, run the copy through the dynamic loader as a last resort;
//   (d) if everything fails, log every error and exit 1.
// The temp copy is removed when the forge exits (best effort).
import { spawn, type ChildProcess } from 'node:child_process';
import { chmodSync, copyFileSync, statSync, unlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const dir = dirname(fileURLToPath(import.meta.url));
const forge = join(dir, 'forge');
// Candidate dynamic-loader paths (distros disagree on the exact one;
// note the `.2` suffix — plain `ld-linux-x86-64.so` usually does not exist).
const LD_CANDIDATES = [
  '/lib64/ld-linux-x86-64.so.2',
  '/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2',
  '/usr/lib64/ld-linux-x86-64.so.2',
  '/usr/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2',
  '/lib64/ld-linux-x86-64.so',
];

const errors: string[] = [];
let tempCopy: string | null = null;
let copyCounter = 0;

function log(msg: string): void {
  process.stderr.write(`[entry] ${msg}\n`);
}

function errText(err: unknown): string {
  const e = err as NodeJS.ErrnoException;
  return `${e.message} (code ${e.code ?? 'unknown'})`;
}

// Spawn a command; resolves with the child once the OS accepted the exec,
// rejects if the exec itself failed (EACCES, ENOENT, ...). A child that
// starts and then exits on its own resolves normally — that is the forge's
// own outcome, not a launch failure.
function trySpawn(cmd: string, args: string[]): Promise<ChildProcess> {
  return new Promise((resolve, reject) => {
    const child = spawn(cmd, args, { stdio: 'inherit', env: process.env });
    let settled = false;
    child.on('error', (err) => {
      if (!settled) {
        settled = true;
        reject(err);
      }
    });
    child.on('spawn', () => {
      if (!settled) {
        settled = true;
        resolve(child);
      }
    });
  });
}

// Wait for a successfully started child and return its exit code.
function waitFor(child: ChildProcess): Promise<number> {
  return new Promise((resolve) => {
    child.on('exit', (code, signal) => {
      cleanupTemp();
      resolve(signal !== null ? 1 : (code ?? 1));
    });
  });
}

function cleanupTemp(): void {
  if (tempCopy !== null) {
    try {
      unlinkSync(tempCopy);
    } catch {
      // Best effort only.
    }
    tempCopy = null;
  }
}

// Copy the binary to a fresh, uniquely named file in os.tmpdir() and make
// the copy executable. Returns the copy's path.
function makeTempCopy(): string {
  copyCounter += 1;
  const copy = join(tmpdir(), `forge-${process.pid}-${copyCounter}`);
  copyFileSync(forge, copy);
  chmodSync(copy, 0o755);
  tempCopy = copy;
  return copy;
}

async function main(): Promise<number> {
  // (a) chmod in place + spawn by absolute path (writable checkout).
  try {
    const mode = statSync(forge).mode & 0o777;
    if ((mode & 0o100) === 0) {
      chmodSync(forge, 0o755);
      log(`forge was mode ${mode.toString(8)}; chmod 0o755 applied`);
    }
  } catch (err) {
    errors.push(`chmod in place: ${errText(err)}`);
    log(`could not chmod forge in place (continuing): ${errText(err)}`);
  }
  try {
    return await waitFor(await trySpawn(forge, []));
  } catch (err) {
    errors.push(`spawn ${forge}: ${errText(err)}`);
    log(`spawn ${forge} failed: ${errText(err)}`);
  }

  // (b) copy to tmpdir, chmod the copy, spawn it (read-only checkout).
  try {
    const copy = makeTempCopy();
    log(`copied forge to ${copy} (chmod 0o755)`);
    return await waitFor(await trySpawn(copy, []));
  } catch (err) {
    errors.push(`tmp copy + spawn: ${errText(err)}`);
    log(`tmp-copy spawn failed: ${errText(err)}`);
    cleanupTemp();
  }

  // (c) last resort: run the copy through the dynamic loader.
  // (The forge is statically linked, so this is unlikely to work — but it
  // costs one exec attempt and may save the trial on a noexec /tmp.)
  // NOTE: unlike (a)/(b), the child here is ld.so, not the forge — so a
  // non-zero exit means the ATTEMPT failed and the next loader is tried.
  let loaderTried = false;
  for (const ld of LD_CANDIDATES) {
    try {
      statSync(ld);
    } catch {
      continue; // loader not at this path on this system
    }
    loaderTried = true;
    let copy: string;
    try {
      copy = makeTempCopy();
    } catch (err) {
      errors.push(`tmp copy for ${ld}: ${errText(err)}`);
      log(`could not copy forge for loader ${ld}: ${errText(err)}`);
      continue;
    }
    log(`trying dynamic loader ${ld} on ${copy}`);
    let code: number;
    try {
      code = await waitFor(await trySpawn(ld, [copy]));
    } catch (err) {
      errors.push(`dynamic loader ${ld}: ${errText(err)}`);
      log(`dynamic-loader ${ld} failed: ${errText(err)}`);
      cleanupTemp();
      continue;
    }
    cleanupTemp();
    if (code === 0) return 0;
    errors.push(`dynamic loader ${ld}: exited with code ${code}`);
    log(`dynamic-loader ${ld} exited with code ${code}; trying next loader`);
  }
  if (!loaderTried) {
    errors.push('dynamic loader: no ld-linux found at any known path');
    log('no dynamic loader found at any known path');
  }

  // (d) nothing worked.
  log(
    `FATAL: could not start the forge by any method. ` +
      `The forge binary must be executable; if this persists, check the file mode of 'forge' in the checkout. ` +
      `Errors: ${errors.join(' | ')}`,
  );
  return 1;
}

process.exit(await main());
