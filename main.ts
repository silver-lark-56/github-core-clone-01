// main.ts: the entry point the trial runs as `node /bot/main.ts`.
// A thin launcher — the whole forge is the Go binary next to it.
// It spawns ./forge, forwards stdio, and exits with the forge's code.
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const dir = dirname(fileURLToPath(import.meta.url));
const forge = join(dir, 'forge');

const child = spawn(forge, [], { stdio: 'inherit', env: process.env });
child.on('error', (err) => {
  process.stderr.write(`[entry] could not start the forge: ${err.message}\n`);
  process.exit(1);
});
child.on('exit', (code, signal) => {
  if (signal !== null) process.exit(1);
  process.exit(code ?? 1);
});
