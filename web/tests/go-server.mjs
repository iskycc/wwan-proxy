import { spawn } from 'node:child_process';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('../../', import.meta.url));
const directory = await mkdtemp(path.join(tmpdir(), 'wwan-ui-integration-'));
const binary = path.join(directory, 'wwan-proxy');
const build = spawn('go', ['build', '-o', binary, './cmd/wwan-proxy'], {
  cwd: root,
  stdio: 'inherit',
});
const code = await new Promise((resolve) => build.once('exit', resolve));
if (code !== 0) {
  await rm(directory, { recursive: true, force: true });
  process.exit(code || 1);
}
const server = spawn(
  binary,
  ['-db', path.join(directory, 'test.db'), '-web-default', '127.0.0.1:4180'],
  { cwd: root, stdio: 'inherit' },
);
for (const signal of ['SIGTERM', 'SIGINT']) process.once(signal, () => server.kill(signal));
server.once('exit', async (code) => {
  await rm(directory, { recursive: true, force: true });
  process.exit(code || 0);
});
