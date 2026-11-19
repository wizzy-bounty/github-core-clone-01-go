// Anvil-Go launcher.
//
// The forge itself is the vendored statically linked binary ./anvil-go
// (linux/amd64, built with CGO_ENABLED=0, Go stdlib only). The platform runs
// `node /bot/main.ts`; this launcher only execs the binary and forwards its
// exit code, so the trial runs the Go forge directly.
//
// The binary owns the whole lifecycle: it binds an ephemeral port, registers
// it with the environment (POST $MAKEFUN_ENV_URL/register), serves the forge
// (git smart HTTP v2/v0, REST v3, GraphQL v4, MCP) until the environment
// reports done (GET $MAKEFUN_ENV_URL/done), then exits 0.
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const dir = path.dirname(fileURLToPath(import.meta.url));
const binary = path.join(dir, 'anvil-go');

const child = spawn(binary, process.argv.slice(2), { stdio: 'inherit' });
child.on('error', (err) => {
  console.error(`anvil-go: failed to start ${binary}: ${err.message}`);
  process.exit(1);
});
child.on('exit', (code, signal) => {
  if (signal) process.exit(1);
  process.exit(code ?? 1);
});
