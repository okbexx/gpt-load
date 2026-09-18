import { readFile } from 'node:fs/promises';
import { createDriver } from './driver.mjs';

try {
  const args = process.argv.slice(2);
  if (args.length && (args.length !== 2 || args[0] !== '--config')) throw new Error('usage');
  const config = args.length ? JSON.parse(await readFile(args[1], 'utf8')) : {};
  if (!config || typeof config !== 'object' || Array.isArray(config) || Object.keys(config).some(k => !['host','port','allowedBaseUrls','limits'].includes(k))) throw new Error('config');
  const host = config.host ?? '127.0.0.1';
  const port = config.port ?? 8788;
  if (!Number.isInteger(port) || port < 0 || port > 65535) throw new Error('port');
  const server = createDriver({ token: process.env.PI_DRIVER_TOKEN, allowedBaseUrls: config.allowedBaseUrls, limits: config.limits });
  server.on('error', () => { console.error('pi-driver: listen failed'); process.exitCode = 1; });
  server.listen(port, host, () => console.log(`pi-driver listening on ${host}:${server.address().port}`));
  const shutdown = () => { server.closeAllConnections(); server.close(); };
  process.once('SIGINT', shutdown);
  process.once('SIGTERM', shutdown);
} catch {
  console.error('pi-driver: invalid configuration; set PI_DRIVER_TOKEN (32+ printable characters), optionally --config FILE');
  process.exitCode = 1;
}
