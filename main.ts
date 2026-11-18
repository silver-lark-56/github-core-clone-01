// main.ts: the entry point the trial runs as `node /bot/main.ts`.
// A thin launcher — the whole forge is the Go binary next to it.
// It spawns ./forge, forwards stdio, and exits with the forge's code.
//
// The binary is shipped in the repository, but the upload paths used to
// push it (connector push + GitHub web upload) strip the executable bit:
// the tree on GitHub records `forge` as mode 100644 even though the local
// file is 100755. Spawning it then fails with EACCES and the forge never
// binds, which the trial reports as working=false. So before spawning we
// chmod the binary 0o755 ourselves (best effort — ignored if it fails,
// e.g. on a read-only checkout) and always spawn by absolute path resolved
// from this file's own location, never from the working directory.
import { spawn } from 'node:child_process';
import { chmodSync, statSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const dir = dirname(fileURLToPath(import.meta.url));
const forge = join(dir, 'forge');

try {
  const mode = statSync(forge).mode & 0o777;
  if ((mode & 0o100) === 0) {
    chmodSync(forge, 0o755);
    process.stderr.write(`[entry] forge was mode ${mode.toString(8)}; chmod 0o755 applied\n`);
  }
} catch (err) {
  // Best effort only: a read-only filesystem or a missing file must not
  // stop the spawn attempt — spawn will report the real problem below.
  process.stderr.write(`[entry] could not chmod forge (continuing anyway): ${(err as Error).message}\n`);
}

const child = spawn(forge, [], { stdio: 'inherit', env: process.env });
child.on('error', (err) => {
  process.stderr.write(
    `[entry] FATAL: could not start the forge at ${forge}: ${err.message} ` +
    `(code ${(err as NodeJS.ErrnoException).code ?? 'unknown'}). ` +
    `The forge binary must be executable; if this persists, check the file mode of 'forge' in the checkout.\n`,
  );
  process.exit(1);
});
child.on('exit', (code, signal) => {
  if (signal !== null) process.exit(1);
  process.exit(code ?? 1);
});
